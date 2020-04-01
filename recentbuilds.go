package main

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
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

	b := bufio.NewReader(f)
	l := []string{}
	targetUse := map[string]int{}
	for {
		s, err := b.ReadString('\n')
		if s != "" {
			s = s[:len(s)-1]
			t := strings.Split(s, " ")
			switch t[0] {
			case "0":
				if len(t) != 14 {
					log.Printf("bad line with v0, got %d tokens, expected 14", len(t))
					return
				}

				req := request{
					Mod:       t[11],
					Version:   t[12],
					Dir:       t[13],
					Goos:      t[8],
					Goarch:    t[9],
					Goversion: t[10],
					Page:      pageIndex,
				}
				if t[1] != "x" {
					sha256, err := hex.DecodeString(t[1])
					if err != nil {
						log.Printf("bad hex sha256 %q: %v", t[1], err)
						return
					}
					sum := "0" + base64.RawURLEncoding.EncodeToString(sha256[:20])
					req.Sum = sum
				}

				l = append(l, req.urlPath())
				availableBuilds.index[req.buildIndexRequest().urlPath()] = req.Sum != ""
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
