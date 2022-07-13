package main

import (
	"fmt"
	"net"

	"rsc.io/quote"
)


func main() {
	ln, err := net.Listen("tcp", ":9000")
	if err != nil {
		// handle error
	}
	for {
		_, err := ln.Accept()
		if err != nil {
			// handle error
		}
		fmt.Println(quote.Go())
	}
}

