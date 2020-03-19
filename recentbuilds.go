package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
)

// Reading recent builds is best-effort...
func readRecentBuilds() {
	f, err := os.Open("data/builds.txt")
	if err != nil {
		log.Printf("open: %v", err)
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		log.Printf("stat: %v", err)
		return
	}
	offset := fi.Size()
	if offset > 1024 {
		offset = 1024
	}
	offset, err = f.Seek(-offset, 2)
	if err != nil {
		log.Printf("seek: %v", err)
		return
	}

	b := bufio.NewReader(f)
	if offset > 0 {
		_, err = b.ReadString('\n')
		if err != nil {
			log.Printf("discard first line: %v", err)
			return
		}
	}
	l := []string{}
	for {
		s, err := b.ReadString('\n')
		if s != "" {
			s = s[:len(s)-1]
			t := strings.Split(s, " ")
			if len(t) == 6 {
				p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", t[0], t[1], t[2], t[3], t[4], t[5])
				l = append(l, p)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("reading line: %v", err)
			return
		}
	}
	if len(l) > 10 {
		l = l[len(l)-10:]
	}
	recentBuilds.paths = l
}
