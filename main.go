package main

import (
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Represents a list of headers given on the command line
type headersSlice []string

var port int
var skipLookup bool
var buffer bool
var additionalHeaders headersSlice
var index bool
var logRequests bool
var upload bool

// Template used for directory listings
var dirListTemplate = template.Must(template.New("dirlist").Parse(`
<html>
<head>
  <meta charset="utf-8" />
  <title>{{ .Path }}</title>
  <style>
    body {
      font-family: "Open Sans", Helvetica, Sans;
    }
    table {
      border: none;
      border-collapse: collapse;
      text-align: left;
    }
    th, td {
      padding-right: 3rem;
    }
    td.size {
      text-align: right;
    }
    tbody {
      font-family: monospace;
    }
  </style>
</head>
<body>
  <h1>Directory listing for {{ .Path }}</h1>
{{- if .Upload }}
  <form action="" method="post" enctype="multipart/form-data">
    <input type="file" name="file" id="file" multiple>
    <input type="submit" value="Upload">
  </form>
{{- end }}
  <table>
    <thead>
      <th>Name</th>
      <th>Size</th>
      <th>Modified</th>
      <th></th>
    </thead>
    <tbody>
{{- if .Parent }}
      <tr>
        <td><a href="{{ .Parent }}">../</a></td>
        <td></td>
        <td></td>
      </tr>
{{- end }}
{{- range .Entries }}
      <tr>
        <td>
          <a href="{{ .Path }}">{{ .Name }}</a>
        </td>
        <td class="size">{{ .SizeHuman }}</td>
        <td>{{ .ModTime }}</td>
{{- if .Dir }}
        <td></td>
{{- else }}
        <td>(<a href="{{ .Path }}?download=1">Download</a>)</td>
{{- end }}
      </tr>
{{- end }}
    </tbody>
  </table>
</body>
</html>
`))

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
	flag.BoolVar(&index, "index", false, "generate directory listings")
	flag.BoolVar(&logRequests, "log", false, "log requests")
	flag.BoolVar(&upload, "upload", false, "allow file upload")
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

func contentTypeFromName(baseName string) string {
	contentType := mime.TypeByExtension(filepath.Ext(baseName))
	if contentType == "" {
		return "application/octet-stream"
	}
	return contentType
}

type dir struct {
	path  string
	index bool
}

func (d *dir) addHeaders(headers http.Header, info os.FileInfo, download bool) {
	// Write download information
	if info.IsDir() {
		// For index
		headers.Add("Content-Type", "text/html")
	} else {
		name := info.Name()
		if download {
			disposition := fmt.Sprintf("attachment; filename=\"%s\"", name)
			headers.Add("Content-Disposition", disposition)
			headers.Add("Content-Type", contentTypeFromName(name))
		} else {
			// When previewing a file set text/plain so the browser will display
			// the source rather than interpreting HTML or triggering download
			// for non-supported filetypes.
			headers.Add("Content-Type", "text/plain")
		}
		headers.Add("Content-Length", strconv.FormatInt(info.Size(), 10))
	}

	for _, header := range additionalHeaders {
		parts := strings.SplitN(header, ":", 2)
		headers.Add(parts[0], parts[1])
	}
}

func (d *dir) pathIsUnderRoot(p string) bool {
	if d.path == "/" {
		return true
	}

	if p == d.path {
		return true
	}

	return strings.HasPrefix(p, d.path+"/")
}

func (d *dir) handle(resp http.ResponseWriter, req *http.Request) {
	method := req.Method
	if method != http.MethodGet &&
		method != http.MethodHead &&
		!(upload && method == http.MethodPost) {
		resp.WriteHeader(http.StatusMethodNotAllowed)
		fmt.Fprintln(resp, "method not allowed")
		return
	}

	// d.path is absolute and Join calls Clean for us, so localPath is a clean,
	// absolute path.
	localPath := filepath.Join(d.path, strings.TrimLeft(req.URL.Path, "/"))

	if !d.pathIsUnderRoot(localPath) {
		resp.WriteHeader(http.StatusNotFound)
		fmt.Fprintln(resp, "not found")
		return
	}

	file, err := os.Open(localPath)

	if err != nil {
		if os.IsNotExist(err) {
			resp.WriteHeader(http.StatusNotFound)
			fmt.Fprintln(resp, "not found")
		} else if os.IsPermission(err) {
			resp.WriteHeader(http.StatusForbidden)
			fmt.Fprintln(resp, "permission denied")
		} else {
			resp.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(os.Stderr, "handle: could not open file", err)
			fmt.Fprintln(resp, "could not open file", err)
		}
		return
	}

	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		resp.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(os.Stderr, "handle: could not stat file", err)
		fmt.Fprintln(resp, "could not open file", err)
		return
	}

	// 403 on directories if not returning indexes
	if info.IsDir() && !d.index {
		resp.WriteHeader(http.StatusForbidden)
		fmt.Fprintln(resp, "permission denied")
		return
	}

	// Handle file upload. If uploads are disabled this will have been rejected earlier.
	if method == http.MethodPost {
		uploadErr := func(err error) {
			resp.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(os.Stderr, "handle: could not handle file upload:", err)
			fmt.Fprintln(resp, "could not upload file", err)
		}
		processPart := func(part *multipart.Part) bool {
			var out *os.File
			var err error

			defer part.Close()
			partFileName := part.FileName()
			if partFileName == "" {
				out, err = os.CreateTemp(localPath, "httpf-upload-")
				if err != nil {
					uploadErr(err)
					return false
				}
			} else {
				newFilePath := filepath.Join(localPath, partFileName)
				if !d.pathIsUnderRoot(newFilePath) {
					resp.WriteHeader(http.StatusNotFound)
					fmt.Fprintln(resp, "not found")
					return false
				}
				out, err = os.OpenFile(newFilePath, os.O_WRONLY|os.O_CREATE, 0666)
				if err != nil {
					uploadErr(err)
					return false
				}
			}
			defer out.Close()
			if _, err := io.Copy(out, part); err != nil {
				uploadErr(err)
				return false
			}
			return true
		}
		files, err := req.MultipartReader()
		if err != nil {
			uploadErr(err)
			return
		}
		for {
			part, err := files.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				uploadErr(err)
				return
			}
			if !processPart(part) {
				break
			}
		}
	}

	// Preview or download? In index generating mode we default to previewing.
	var download bool
	if d.index {
		download = req.FormValue("download") != ""
	} else {
		download = req.FormValue("view") == ""
	}

	// Write download information
	d.addHeaders(resp.Header(), info, download)

	// HEAD request. No content returned.
	if method == http.MethodHead {
		return
	}

	if info.IsDir() {
		entries, err := file.Readdir(0)
		if err != nil {
			resp.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintln(os.Stderr, "handle: could not read dir", err)
			fmt.Fprintln(resp, "internal error", err)
			return
		}

		type FileEntry struct {
			Name      string
			Path      string
			Size      int64
			SizeHuman string
			ModTime   string
			Dir       bool
		}

		data := struct {
			Path    string
			Parent  string
			Entries []FileEntry
			Upload  bool
		}{
			Upload: upload,
		}

		data.Path = strings.Trim(req.URL.Path, "/") + "/"
		if data.Path != "/" {
			data.Parent = path.Dir(path.Clean(req.URL.Path))
			if data.Parent != "/" {
				data.Parent += "/"
			}
		}

		for _, e := range entries {
			entry := FileEntry{
				Name: e.Name(),
				Path: path.Join(req.URL.Path, e.Name()),
				Size: e.Size(),
			}

			if e.IsDir() {
				entry.Name += "/"
				entry.Path += "/"
				entry.Dir = true
			}

			size := entry.Size
			unit := ""
			switch {
			case size < 1024:
			case size < 1024*1024:
				size /= 1024
				unit = "k"
			case size < 1024*1024*1024:
				size /= 1024 * 1024
				unit = "M"
			default:
				size /= 1024 * 1024 * 1024
				unit = "G"
			}

			entry.SizeHuman = fmt.Sprintf("%d%s", size, unit)
			entry.ModTime = e.ModTime().Format(time.RFC3339)

			data.Entries = append(data.Entries, entry)
		}

		sort.Slice(data.Entries, func(i, j int) bool {
			return data.Entries[i].Name < data.Entries[j].Name
		})

		err = dirListTemplate.Execute(resp, data)

		if err != nil {
			fmt.Fprintln(os.Stderr, "handle: failed to send index", err)
		}
	} else {
		// Copy file content
		_, err = io.Copy(resp, file)
		if err != nil {
			fmt.Fprintln(os.Stderr, "handle: failed to send file", err)
		}
	}
}

func newDirHandler(filename string) http.HandlerFunc {
	abs, err := filepath.Abs(filename)
	if err != nil {
		fail(err)
	}

	d := &dir{abs, index}
	return d.handle
}

type file struct {
	filename    string
	baseName    string
	contentType string
}

func (f *file) addHeaders(headers http.Header) {
	// Write download information
	disposition := fmt.Sprintf("attachment; filename=\"%s\"", f.baseName)
	headers.Add("Content-Disposition", disposition)
	headers.Add("Content-Type", f.contentType)
	for _, header := range additionalHeaders {
		parts := strings.SplitN(header, ":", 2)
		headers.Add(parts[0], parts[1])
	}
}

func (f *file) handle(resp http.ResponseWriter, req *http.Request) {
	file, err := os.Open(f.filename)

	if err != nil {
		resp.WriteHeader(500)
		fmt.Fprintln(os.Stderr, "handle: could not open file", err)
		fmt.Fprintln(resp, "could not open file", err)
		return
	}

	defer file.Close()

	// Write download information
	f.addHeaders(resp.Header())

	// Copy file content
	_, err = io.Copy(resp, file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "handle: failed to send file", err)
	}
}

type bufferedFile struct {
	*file
	data []byte
}

func (f *bufferedFile) handle(resp http.ResponseWriter, req *http.Request) {
	// Write download information
	f.addHeaders(resp.Header())

	// Copy file content
	_, err := resp.Write(f.data)
	if err != nil {
		fmt.Fprintln(os.Stderr, "handle: failed to send file", err)
	}
}

func newFileHandler(filename string, buffer bool) http.HandlerFunc {
	// Keep a copy of the filename without directory
	baseName := filepath.Base(filename)

	// Try to send an appropriate mime-type
	contentType := contentTypeFromName(baseName)

	f := &file{filename, baseName, contentType}

	// Test file can be opened
	file, err := os.Open(filename)
	if err != nil {
		fail("could not open file:", err)
	}
	defer file.Close()

	if !buffer {
		return f.handle
	}

	data, err := ioutil.ReadAll(file)
	if err != nil {
		fail("could not buffer file:", err)
	}

	bf := &bufferedFile{f, data}
	return bf.handle
}

type LoggingResponseWriter struct {
	http.ResponseWriter

	request *http.Request
	logged  bool
}

func (l *LoggingResponseWriter) log(statusCode int) {
	if l.logged {
		return
	}

	l.logged = true
	log.Printf("%s %s %s → %d",
		l.request.Method,
		l.request.URL,
		l.request.Proto,
		statusCode,
	)
}

func (l *LoggingResponseWriter) Write(data []byte) (int, error) {
	l.log(http.StatusOK)
	return l.ResponseWriter.Write(data)
}

func (l *LoggingResponseWriter) WriteHeader(statusCode int) {
	l.log(statusCode)
	l.ResponseWriter.WriteHeader(statusCode)
}

func addLogging(handler http.HandlerFunc) http.HandlerFunc {
	return func(resp http.ResponseWriter, req *http.Request) {
		resp = &LoggingResponseWriter{
			ResponseWriter: resp,
			request:        req,
			logged:         false,
		}
		handler(resp, req)
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

	var filename string
	switch flag.NArg() {
	case 0:
		filename = "."
	case 1:
		filename = flag.Arg(0)
	default:
		fail("too many arguments")
	}

	fi, err := os.Stat(filename)
	if err != nil {
		fail("failed to stat file:", err)
	}

	var handle http.HandlerFunc
	if fi.IsDir() {
		handle = newDirHandler(filename)
	} else {
		handle = newFileHandler(filename, buffer)
	}

	if logRequests {
		handle = addLogging(handle)
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
	if fi.IsDir() {
		fmt.Printf("http://%s:%s/\n", addr, port)
	} else {
		fmt.Printf("http://%s:%s/%s\n", addr, port, fi.Name())
	}

	// Serve requests
	log.Fatal(http.Serve(nl, http.HandlerFunc(handle)))
}
