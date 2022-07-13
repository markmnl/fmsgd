package main

import (
	"fmt"
	"log"
	"net"
	"unicode/utf8"
)

const ReadBufferSize = 8192 

func handleConn(c net.Conn) {
	log.Printf("Connection from: %s", c.RemoteAddr().String())
	var buffer [ReadBufferSize]byte
	var quit bool = false
	for {
		var slice = buffer[:]
		n, err := c.Read(slice)
		if err != nil {
			log.Printf("Error reading from connection %s: %s\n", c.RemoteAddr().String(), err)
			c.Close()
			break
		}
		for i := 0; i < n; i++ {
			r, size := utf8.DecodeRune(slice)
			slice = slice[size:]
			fmt.Println(string(r))
			if string(r) == "q" {
				quit = true
				break
			}
		}
		if quit {
			c.Close()
			log.Printf("Quitting %s\n", c.RemoteAddr().String())
			return
		}
	}
}


func main() {
	ln, err := net.Listen("tcp", "localhost:9000")
	if err != nil {
		log.Fatal(err)
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("Error accepting connection from: %s\n", ln.Addr().String())
		} else {
			go handleConn(conn)
		}
	}
}

