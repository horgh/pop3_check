/*
 * connect to a pop3 mailbox and look at the message list.
 * if there is a message size above a certain threshold, output
 * a warning.
 *
 * why? because I have a pop3 mailbox that gets polled by gmail
 * which then downloads all the messages. however it appears gmail
 * will not download messages if they are above a certain size
 * and the mailbox can then fill up leading to message rejection.
 * this is to notify about that situation.
 */
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"
	"time"
)

type argDef struct {
	Host  string
	User  string
	Pass  string
	Size  int
	Quota int
}

type Conn struct {
	// ReadWriter is a wrapper around Conn
	ReadWriter *bufio.ReadWriter
	// Conn is the actual connection.
	Conn net.Conn
}

// deadline for read timeouts.
var readDeadline = 5 * time.Second

// whether verbose output on or not.
var verboseOutput = false

// port to connect to.
var connectPort = 110

// read contents of a file.
func readFile(path string) (string, error) {
	if len(path) == 0 {
		return "", errors.New("invalid path")
	}
	fi, err := os.Open(path)
	if err != nil {
		return "", err
	}
	reader := bufio.NewReader(fi)
	contents := ""
	for {
		// TODO: what encoding is this defaulting to?
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			fi.Close()
			return "", err
		}
		line = strings.TrimSpace(line)
		contents += line
	}
	fi.Close()
	return contents, nil
}

// getArgs retrieves and validates command line arguments
func getArgs() (*argDef, error) {
	host := flag.String("host", "", "POP3 server host")
	user := flag.String("user", "", "POP3 username")
	passFile := flag.String("password-file", "", "POP3 password can be found in this file")
	size := flag.Int("size", 5*1024*1024, "Message size (bytes) above which to warn.")
	quota := flag.Int("quota", 10*1024*1024, "Size in bytes to above which to warn if the total size of all messages in the mailbox exceeds. This is to warn if we begin to reach quota due to many smaller messages.")
	verbose := flag.Bool("verbose", false, "Verbose output or not.")
	flag.Parse()
	if len(*host) == 0 {
		errString := "You must provide a host."
		log.Print(errString)
		flag.PrintDefaults()
		return nil, errors.New(errString)
	}
	// TODO: better host validation
	if len(*user) == 0 {
		errString := "You must provide a username."
		log.Print(errString)
		flag.PrintDefaults()
		return nil, errors.New(errString)
	}
	if len(*passFile) == 0 {
		errString := "You must provide a password file."
		log.Print(errString)
		flag.PrintDefaults()
		return nil, errors.New(errString)
	}
	pass, err := readFile(*passFile)
	if err != nil {
		errString := fmt.Sprintf("Unable to read password file: %s",
			err.Error())
		log.Print(errString)
		flag.PrintDefaults()
		return nil, errors.New(errString)
	}
	if *size <= 0 {
		errString := "You must provide a size larger than zero."
		log.Print(errString)
		flag.PrintDefaults()
		return nil, errors.New(errString)
	}
	if *quota <= 0 {
		errString := "You must provide a quota larger than zero."
		log.Print(errString)
		flag.PrintDefaults()
		return nil, errors.New(errString)
	}
	if *verbose {
		verboseOutput = true
	}
	return &argDef{
		Host:  *host,
		User:  *user,
		Pass:  pass,
		Size:  *size,
		Quota: *quota,
	}, nil
}

// newConn creates a new Conn.
func newConn(conn net.Conn) *Conn {
	// set up our buffered reader/writer.
	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	rw := bufio.NewReadWriter(reader, writer)
	return &Conn{
		ReadWriter: rw,
		Conn:       conn,
	}
}

// Close closes a connection.
func (conn *Conn) Close() {
	conn.Conn.Close()
}

// setReadDeadline sets a read deadline on a connection.
// this is something we will do often so contain it in a function.
func (conn *Conn) setReadDeadline() {
	conn.Conn.SetReadDeadline(time.Now().Add(readDeadline))
}

// readLine reads a single line from the connection.
func (conn *Conn) readLine() (string, error) {
	// set a read deadline.
	conn.setReadDeadline()
	// read the string.
	line, err := conn.ReadWriter.Reader.ReadString('\n')
	if err != nil {
		log.Printf("Read failure: %s", err.Error())
		return "", err
	}
	// TODO: this will trim off all leading/trailing space characters.
	//   if we want to validate exact grammar validity we might want to ensure
	//   that we have exactly one \r\n trailing.
	line = strings.TrimSpace(line)
	return line, nil
}

// readLines() reads lines until EOF/timeout/error.
// endCheck is a function that we will run on each line that
// will tell us we can return if it returns true.
func (conn *Conn) readLines(endCheck func(string) bool) ([]string, error) {
	// the connection should close automatically (or rather, ARIN's does)
	// after it sends us its response.
	// we will read lines until we reach EOF or time out.
	var lines []string
	for {
		line, err := conn.readLine()
		if err != nil {
			// if we receive a timeout or eof then we can process what we have
			// since these errors are what we want (in theory).
			// a timeout is acceptable as we keep trying to read even after the
			// server has sent its last line, so we will time out there.
			netError, ok := err.(net.Error)
			if ok && netError.Timeout() {
				break
			}
			if err == io.EOF {
				break
			}
			log.Printf("Read error: %s", err.Error())
			return nil, err
		}
		lines = append(lines, line)
		// we might be able to know to abort (and not wait on timeout).
		if endCheck(line) {
			return lines, nil
		}
	}
	return lines, nil
}

// writeLine() writes a line to the connection.
// we write the line and add CRLF, and then flush the buffer.
func (conn *Conn) writeLine(s string) error {
	if verboseOutput {
		log.Printf("Writing line [%s]", s)
	}
	_, err := conn.ReadWriter.Writer.WriteString(s + "\r\n")
	if err != nil {
		log.Printf("Failure writing: %s", err.Error())
		return err
	}
	err = conn.ReadWriter.Writer.Flush()
	if err != nil {
		log.Printf("Flush error: %s", err.Error())
		return err
	}
	return nil
}

// checkMailbox connects to a POP3 mailbox and lists the messages.
// if any are above the given warning size then log a warning.
func checkMailbox(host string, user string, pass string, warnSize int,
	quotaWarnSize int) error {
	// connect
	if verboseOutput {
		log.Printf("Connecting to %s...", host)
	}
	hostPort := fmt.Sprintf("%s:%d", host, connectPort)
	rawConn, err := net.Dial("tcp4", hostPort)
	if err != nil {
		log.Printf("Failed to connect to [%s]: %s", hostPort,
			err.Error())
		return err
	}
	if verboseOutput {
		log.Printf("Connected to [%s] (%s)", hostPort,
			rawConn.RemoteAddr().String())
	}
	conn := newConn(rawConn)

	// ensure we get OK line before we do anything.
	lines, err := conn.readLines(func(s string) bool {
		return strings.HasPrefix(s, "+OK")
	})
	if err != nil {
		log.Printf("Error reading lines: %s", err.Error())
		return err
	}
	if len(lines) != 1 {
		log.Printf("Unexpected number of lines: %s", len(lines))
		return errors.New("Unexpected line count")
	}
	if !strings.HasPrefix(lines[0], "+OK ") {
		log.Printf("Greeting line is not OK: %s", lines[0])
		return errors.New("Invalid greeting")
	}

	// try to log in now.
	s := fmt.Sprintf("USER %s", user)
	err = conn.writeLine(s)
	if err != nil {
		log.Printf("Login failure: user: %s", err.Error())
		return err
	}
	// check OK.
	lines, err = conn.readLines(func(s string) bool {
		return strings.HasPrefix(s, "+OK")
	})
	if err != nil {
		log.Printf("Failed to read lines: %s", err.Error())
		return err
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "+OK") {
		log.Printf("Unexpected USER response")
		return errors.New("Unexpected user respose")
	}
	// password.
	s = fmt.Sprintf("PASS %s", pass)
	err = conn.writeLine(s)
	if err != nil {
		log.Printf("Login failure: pass: %s", err.Error())
		return err
	}
	// check OK.
	lines, err = conn.readLines(func(s string) bool {
		return strings.HasPrefix(s, "+OK")
	})
	if err != nil {
		log.Printf("Failed to read lines: pass: %s", err.Error())
		return err
	}
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "+OK") {
		log.Printf("Unexpected PASS response")
		return errors.New("Unexpected PASS respose")
	}

	// now list messages.
	err = conn.writeLine("LIST")
	lines, err = conn.readLines(func(s string) bool {
		return s == "."
	})
	if err != nil {
		log.Printf("Failed to list messages: %s", err.Error())
		return err
	}
	sizeOfAllMessages := 0
	for _, line := range lines {
		if verboseOutput {
			log.Printf("Read LIST line: %s", line)
		}

		// +OK is the first line.
		// . is the end line.
		// all others are messages.
		if strings.HasPrefix(line, "+OK") || line == "." {
			continue
		}

		var id int
		var size int
		countScanned, err := fmt.Sscanf(line, "%d %d", &id, &size)
		if err != nil {
			log.Printf("LIST line parse failure: %s", err.Error())
			return errors.New("Unable to parse LIST line")
		}
		if countScanned != 2 {
			log.Printf("LIST line not fully parsed: %s", line)
			return errors.New("Failed to parse LIST line")
		}

		if size > warnSize {
			log.Printf("Warning: Message %d has size %d",
				id, size)
		}
		sizeOfAllMessages += size
	}

	if verboseOutput {
		log.Printf("Total size of mailbox: %d", sizeOfAllMessages)
	}
	if sizeOfAllMessages > quotaWarnSize {
		log.Printf("Warning: Mailbox has total used size: %d", sizeOfAllMessages)
	}
	return nil
}

func main() {
	log.SetFlags(log.Ltime)
	args, err := getArgs()
	if err != nil {
		os.Exit(1)
	}
	err = checkMailbox(args.Host, args.User, args.Pass, args.Size, args.Quota)
	if err != nil {
		os.Exit(1)
	}
}
