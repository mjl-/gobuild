package main

import (
	"context"
	"log"
	"strings"
)

// Reading recent builds is best-effort.
// Called at startup to read recent builds.
// It reads the 1000 most recent records, marks them in targets.use, then sorts the targets.
// It keeps the last 10 builds in memory, for display on the front page.
func readRecentBuilds() {
	n, err := treeSize()
	if err != nil {
		log.Printf("getting sum tree size: %v", err)
		return
	}

	first := int64(0)
	if n > 1000 {
		first = n - 1000
		n = 1000
	}

	if n == 0 {
		return
	}

	records, err := server{}.ReadRecords(context.Background(), first, n)
	if err != nil {
		log.Printf("reading records: %v", err)
		return
	}

	for _, record := range records {
		t := strings.Split(string(record), " ")
		if len(t) != 8 {
			log.Printf("bad record, got %d elements, expected 8: %s", len(t), string(record))
		}

		targets.use[t[3]+"/"+t[4]]++
	}
	targets.sort()

	if len(records) > 10 {
		records = records[len(records)-10:]
	}
	l := []string{}
	for _, record := range records {
		s := string(record)
		s = strings.TrimRight(s, "\n")
		t := strings.Split(s, " ")
		req := request{
			t[0],
			t[1],
			t[2][1:],
			t[3],
			t[4],
			t[5],
			pageIndex,
			t[7],
		}
		l = append(l, req.urlPath())
	}
	recentBuilds.paths = l

}
