package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Represents a list of headers given on the command line
type headersSlice []string

var port int
var skipLookup bool
var buffer bool
var additionalHeaders headersSlice
var filename string
var baseName string
var contentType string
var data []byte = nil

func (headers *headersSlice) String() string {
	return strings.Join(*headers, ",")
}

func (headers *headersSlice) Set(arg string) error {
	*headers = append(*headers, arg)
	return nil
}

func init() {
	flag.IntVar(&port, "port", 0, "listening port, or 0 to use any free port")
	flag.BoolVar(&skipLookup, "n", false, "do not attempt address resolution")
	flag.BoolVar(&buffer, "buffer", false, "buffer the file in memory")
	flag.Var(&additionalHeaders, "header", "additional header(s)")
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

func handleFile(resp http.ResponseWriter, req *http.Request) {
	file, err := os.Open(filename)

	if err != nil {
		resp.WriteHeader(500)
		fmt.Fprintln(os.Stderr, "handle: could not open file", err)
		fmt.Fprintln(resp, "could not open file", err)
		return
	}

	defer file.Close()

	// Write download information
	addHeaders(resp.Header())

	// Copy file content
	_, err = io.Copy(resp, file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "handle: failed to send file", err)
	}
}

func handleBuffered(resp http.ResponseWriter, req *http.Request) {
	// Write download information
	addHeaders(resp.Header())

	// Copy file content
	_, err := resp.Write(data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "handle: failed to send file", err)
	}
}

func addHeaders(headers http.Header) {
	// Write download information
	disposition := fmt.Sprintf("attachment; filename=\"%s\"", baseName)
	headers.Add("Content-Disposition", disposition)
	headers.Add("Content-Type", contentType)
	for _, header := range additionalHeaders {
		parts := strings.SplitN(header, ":", 2)
		headers.Add(parts[0], parts[1])
	}
}

func main() {
	flag.Parse()

	// Check headers
	for _, header := range additionalHeaders {
		if strings.Index(header, ":") < 1 {
			fail("bad -header argument")
		}
	}

	if flag.NArg() != 1 {
		fail("filename required")
	}

	filename = flag.Arg(0)

	// Test file can be opened
	file, err := os.Open(filename)
	if err != nil {
		fail("could not open file:", err)
	}

	handle := handleFile
	if buffer {
		handle = handleBuffered
		data, err = ioutil.ReadAll(file)
		if err != nil {
			fail("could not buffer file:", err)
		}
	}

	file.Close()

	// Keep a copy of the filename without directory
	baseName = filepath.Base(filename)

	// Try to send an appropriate mime-type
	contentType = mime.TypeByExtension(filepath.Ext(baseName))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

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

	if !skipLookup {
		if names, err := net.LookupAddr(addr); err == nil {
			if len(names) > 0 {
				addr = names[0]
				addr = strings.TrimRight(addr, ".")
			}
		}
	}

	_, port, _ := net.SplitHostPort(nl.Addr().String())
	fmt.Printf("http://%s:%s/%s\n", addr, port, baseName)

	// Serve requests
	log.Fatal(http.Serve(nl, http.HandlerFunc(handle)))
}
