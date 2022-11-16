package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"time"
	"unicode/utf8"
)

const ReadBufferSize = 8192 
const (
	HasPid uint8 = 1
	Important uint8 = 1 << 1
)

var IoDeadline, _ = time.ParseDuration("5s")
var AtRune, _ = utf8.DecodeRuneInString("@")

type FMsgAddress struct {
	User string
	Domain string
}

type FMsgAttachmentHeader struct {
	Filename string
	Size uint32
	Data []byte // TODO path instead of in memory
}

type FMsgHeader struct {
	Version uint8
	Flags uint8
	Pid [32]byte
	From FMsgAddress
	To FMsgAddress
	Timestamp float64
	Topic string
	Type string
	Msg []byte // TODO path instead of in memory

}

func parseAddress(b []byte) (FMsgAddress, error) {
	var addr = FMsgAddress{}
	addrStr := string(b)
	firstAt := strings.IndexRune(addrStr, AtRune)
	if firstAt == -1 || firstAt != 0 {
		return addr, fmt.Errorf("invalid address, must start with @ %s", addr)
	}
	lastAt := strings.LastIndex(addrStr, "@")
	if lastAt == firstAt {
		return addr, fmt.Errorf("invalid address, must have second @ %s", addr)
	}
	addr.User = addrStr[1:lastAt]
	addr.Domain = addrStr[lastAt+1:]
	return addr, nil
}

func readHeader(c net.Conn) (FMsgHeader, error) {
	b := make([]byte, ReadBufferSize)
	var h FMsgHeader

	// set read deadline TODO update after reading header
	c.SetReadDeadline(time.Now().Add(IoDeadline))

	// read version and flags
	count, err := io.ReadAtLeast(c, b, 3)
	if err != nil {
		log.Printf("Error reading header from %s: %s", c.RemoteAddr().String(), err)
		return h, err
	}
	h.Version = b[0]
	flags := b[1]
	i := 2

	// TODO if version == 255 this is a CHALLENGE

	// read pid if any
	if flags & HasPid == 1 {  
		diff := count - i
		if diff < 32 {
			n, err := io.ReadAtLeast(c, b[i:], diff)
			if err != nil {
				log.Printf("Error reading pid from %s: %s", c.RemoteAddr().String(), err)
				return h, err
			} else { 
				count += n
			}
		}
		copy(h.Pid[:], b[i:i+32])
		i += 32
		// TODO verify pid
	}

	// read from address
	size := int(b[i])
	i++
	diff := count - i
	if diff < size {
		n, err := io.ReadAtLeast(c, b[i:], diff)
		if err != nil {
			log.Printf("Error reading from address %s: %s", c.RemoteAddr().String(), err)
			return h, err
		}
		count += n
	}
	from, err := parseAddress(b[i:(i+size)])
	if err != nil {
		log.Printf("Error parsing from address %s: %s", c.RemoteAddr().String(), err)
		return h, err
	}
	i += size

	log.Printf("@%s@%s", from.User, from.Domain)
	
	// read to addresse(s)
	// timestamp
	// topic
	// type
	// challenge

	return h, nil
}

func handleConn(c net.Conn) {
	log.Printf("Connection from: %s", c.RemoteAddr().String())
	header, err := readHeader(c)

	if err != nil {
		c.Close()
		log.Printf("Closed: %s", c.RemoteAddr().String())
		return
	}

	resp := fmt.Sprintf("Read header from: %s", header.From)
	resp_bytes := []byte(resp)
	c.Write(resp_bytes)
	log.Println("Voila!")

	// CHALLENGE
	// CHALLENGE RESP
	// checks
	// download
	// close

}


func main() {
	ln, err := net.Listen("tcp", "localhost:36900")
	if err != nil {
		log.Fatal(err)
	}
	for {
		log.Println("Listening on localhost:36900")
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Error accepting connection from, %s: %s\n", ln.Addr().String(), err)
		} else {
			go handleConn(conn)
		}
	}
}

