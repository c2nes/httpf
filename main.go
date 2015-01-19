package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

var port int
var filename string
var baseName string

func init() {
	flag.IntVar(&port, "port", 0, "listening port, or 0 to use any free port")
}

func fail(msg ...interface{}) {
	fmt.Fprintln(os.Stderr, msg...)
	os.Exit(1)
}

func publicAddress() (string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}

	var ips []net.IP

	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			return "", err
		}

		// we don't want to use loopback or multicast address
		if ip.IsGlobalUnicast() {
			ips = append(ips, ip)
		}
	}

	// no good address found...
	if len(ips) == 0 {
		return "", errors.New("no addresses found")
	}

	// try to find an IPv4 address first
	for _, ip := range ips {
		ip = ip.To4()
		if ip != nil {
			return ip.String(), nil
		}
	}

	// otherwise, return whatever we have
	return ips[0].String(), nil
}

func handle(resp http.ResponseWriter, req *http.Request) {
	file, err := os.Open(filename)
	if err != nil {
		resp.WriteHeader(500)
		fmt.Fprintln(os.Stderr, "handle: could not open file", err)
		fmt.Fprintln(resp, "could not open file", err)
	}

	defer file.Close()

	// Write download information
	disposition := fmt.Sprintf("attachment; filename=\"%s\"", baseName)
	resp.Header().Add("Content-Disposition", disposition)

	// Try to send an appropriate mime-type
	contentType := mime.TypeByExtension(filepath.Ext(baseName))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	resp.Header().Add("Content-Type", contentType)

	// Copy file content
	_, err = io.Copy(resp, file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "handle: failed to send file")
	}
}

func main() {
	flag.Parse()

	if flag.NArg() != 1 {
		fail("filename required")
	}

	filename = flag.Arg(0)

	// Test file can be opened
	file, err := os.Open(filename)
	if err != nil {
		fail("could not open file:", err)
	}
	file.Close()

	// Keep a copy of the filename without directory
	baseName = filepath.Base(filename)

	// Start listening for connections
	laddr := fmt.Sprintf(":%d", port)
	nl, err := net.Listen("tcp", laddr)
	if err != nil {
		fail("failed to bind to port:", err)
	}

	// Confirm our listening address
	addr, err := publicAddress()
	if err != nil {
		fail("could not find public address:", err)
	}

	_, port, _ := net.SplitHostPort(nl.Addr().String())
	fmt.Printf("http://%s:%s/%s\n", addr, port, baseName)

	// Serve requests
	log.Fatal(http.Serve(nl, http.HandlerFunc(handle)))
}