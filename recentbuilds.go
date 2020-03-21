package main

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"sort"
	"strings"
)

// Reading recent builds is best-effort...
func readRecentBuilds() {
	f, err := os.Open(path.Join(config.DataDir, "builds.txt"))
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
	targetUse := map[string]int{}
	for {
		s, err := b.ReadString('\n')
		if s != "" {
			s = s[:len(s)-1]
			t := strings.Split(s, " ")
			switch t[0] {
			case "v0":
				if len(t) != 14 {
					log.Printf("bad line with v0, got %d tokens, expected 14", len(t))
					return
				}

				p := fmt.Sprintf("%s-%s-%s/%s@%s/%s", t[8], t[9], t[10], t[11], t[12], t[13])
				var fp string
				if t[1] == "x" {
					fp = "/x/" + p
				} else {
					sha256, err := hex.DecodeString(t[1])
					if err != nil {
						log.Printf("bad hex sha256 %q: %v", t[1], err)
						return
					}
					sum := base64.RawURLEncoding.EncodeToString(sha256[:20])
					fp = "/z/" + sum + "/" + p
				}
				l = append(l, fp)
				availableBuilds.index[p] = t[1] != "x"
				targetUse[t[8]+"/"+t[9]]++
			default:
				log.Printf("bad line, starts with %q", t[0])
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

	sort.Slice(targets, func(i, j int) bool {
		return targetUse[targets[i].osarch()] > targetUse[targets[j].osarch()]
	})
}
