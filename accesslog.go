package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type logResponseWriter struct {
	lh      *logHandler
	Start   time.Time
	Request *http.Request
	Logged  bool // Whether this request has been logged.
	http.ResponseWriter
}

func (lw *logResponseWriter) Write(buf []byte) (int, error) {
	if !lw.Logged {
		lw.LogStatus(200)
	}
	return lw.ResponseWriter.Write(buf)
}

func (lw *logResponseWriter) WriteHeader(statusCode int) {
	if !lw.Logged {
		lw.LogStatus(statusCode)
	}
	lw.ResponseWriter.WriteHeader(statusCode)
}

func (lw *logResponseWriter) Flush() {
	if f, ok := lw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	} else {
		log.Printf("ResponseWriter not a http.Flusher")
	}
}

func noctl(s string) string {
	var r string
	for i, c := range s {
		if c >= ' ' && r == "" {
			continue
		}
		if i > 0 && r == "" {
			r = s[:i]
		}
		if c < ' ' {
			r += fmt.Sprintf(`\x%02x`, c)
		} else {
			r += string(c)
		}
	}
	if r != "" {
		return r
	}
	return s
}

func (lw *logResponseWriter) LogStatus(statusCode int) {
	lw.Logged = true

	now := time.Now()
	ms := now.Sub(lw.Start).Milliseconds()
	text := fmt.Sprintf("%s %d %d %s %s %s %q\n", now.Format(time.RFC3339), ms, statusCode, lw.Request.RemoteAddr, noctl(lw.Request.Method), noctl(lw.Request.RequestURI), noctl(lw.Request.UserAgent()))
	line := logLine{text: text}
	line.date.year, line.date.month, line.date.day = now.Date()

	// Attempt to send. But don't block the http response when writing logs is slow. Just drop logs in that case.
	select {
	case lw.lh.logc <- line:
	default:
	}
}

type logLine struct {
	text string
	date date
}

type date struct {
	year  int
	month time.Month
	day   int
}

type logHandler struct {
	h    http.Handler
	dir  string
	logc chan logLine
}

func (lh *logHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	lw := &logResponseWriter{lh, time.Now(), r, false, w}
	lh.h.ServeHTTP(lw, r)
}

func newLogHandler(h http.Handler, dir string) *logHandler {
	logc := make(chan logLine, 1024)
	lh := &logHandler{h, dir, logc}
	go accessLogger(dir, logc)
	return lh
}

func accessLogger(dir string, logc chan logLine) {
	var file *os.File
	var fileDate date

	writeLog := func(lines []logLine) {
		if file == nil || lines[0].date != fileDate {
			if file != nil {
				if err := file.Close(); err != nil {
					log.Printf("closing access log file: %v", err)
				}
			}

			d := lines[0].date
			p := filepath.Join(dir, fmt.Sprintf("accesslog-%d%02d%02d.log", d.year, d.month, d.day))
			if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0666); err != nil {
				log.Printf("creating access log file %s: %v", p, err)
				return
			} else {
				file = f
				fileDate = d
			}
		}
		b := &strings.Builder{}
		for _, l := range lines {
			b.WriteString(l.text)
		}
		if _, err := file.Write([]byte(b.String())); err != nil {
			log.Printf("writing access log: %v", err)
		}
	}

	// We write lines as fast as possible. When we have a first line, we try to gather
	// as many as we can that fit into the same file, batching them into a single
	// write.
	for {
		line := <-logc
		lines := []logLine{line}
		lastLine := line

	gather:
		for {
			select {
			case line := <-logc:
				if line.date != lastLine.date {
					writeLog(lines)
					lines = nil
				}
				lines = append(lines, line)
				lastLine = line
			default:
				break gather
			}
		}
		writeLog(lines)
	}
}
