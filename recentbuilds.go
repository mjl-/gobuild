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
			switch t[0] {
			case "v1":
				if len(t) != 13 {
					log.Println("bad line with v1, got %d tokens, expected 13", len(t))
					return
				}

				p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", t[7], t[8], t[9], t[10], t[11], t[12])
				l = append(l, p)
				availableBuilds.index[p] = struct{}{}
			default:
				log.Println("bad line, starts with %q", t[0])
				return
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
