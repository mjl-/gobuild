package main

import (
	"context"
	"log"
)

// Called at startup to read recent builds.
// It reads the 1000 most recent records, marks them in targets.use, then sorts the targets.
// It keeps the last 10 builds in memory, for display on the front page.
func readRecentBuilds() {
	n, err := treeSize()
	if err != nil {
		log.Fatalf("getting sum tree size: %v", err)
	}

	first := int64(0)
	if n > 1000 {
		first = n - 1000
		n = 1000
	}

	if n == 0 {
		return
	}

	records, err := serverOps{}.ReadRecords(context.Background(), first, n)
	if err != nil {
		log.Fatalf("reading records: %v", err)
	}

	links := []string{}
	keepFrom := max(len(records)-10, 0)
	for i, record := range records {
		br, err := parseRecord(record)
		if err != nil {
			log.Fatalf("bad record: %v", err)
		}

		targets.use[br.Goos+"/"+br.Goarch]++

		if i < keepFrom {
			continue
		}
		link := request{br.buildSpec, br.Sum, pageIndex}.link()
		links = append(links, link)
	}
	targets.sort()

	recentBuilds.links = links
}
