# httpf

Serves a file over HTTP.

## Installation

```
go get github.com/c2nes/httpf
```

## Usage

```
$ cat test-file.txt
Hello, world!

$ httpf test-file.txt
http://192.168.119.44:59269/test-file.txt

```

```
$ curl -s http://192.168.119.44:59269/test-file.txt
Hello, world!

$
```

