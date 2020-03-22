package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
)

type kind string

const (
	kindQueuePosition = kind("QueuePosition")
	kindTempFail      = kind("TempFailed")
	kindPermFail      = kind("PermFailed")
	kindSuccess       = kind("Success")
)

type buildUpdateMsg struct {
	Kind          kind
	QueuePosition *int       `json:",omitempty"`
	Error         string     `json:",omitempty"`
	Result        *buildJSON `json:",omitempty"`
}

func (bum buildUpdateMsg) json() []byte {
	event, err := json.Marshal(bum)
	if err != nil {
		// should never fail...
		panic(fmt.Sprintf("json marshal of buildUpdateMsg: %v", err))
	}
	return []byte(fmt.Sprintf("event: update\ndata: %s\n\n", event))
}

type buildUpdate struct {
	req           request
	done          bool       // If true, build finished, failure or success.
	err           error      // If not nil, build failed.
	result        *buildJSON // Only in case of success.
	queuePosition int        // If 0, no longer waiting but building.
	msg           []byte     // JSON-encoded buildUpdateMsg to write to clients.
}

type buildRequest struct {
	req    request
	eventc chan buildUpdate
}

var coordinate = struct {
	register   chan buildRequest
	unregister chan buildRequest
}{
	make(chan buildRequest, 1),
	make(chan buildRequest, 1),
}

func registerBuild(req request, eventc chan buildUpdate) {
	coordinate.register <- buildRequest{req, eventc}
}

func unregisterBuild(req request, eventc chan buildUpdate) {
	coordinate.unregister <- buildRequest{req, eventc}
}

func coordinateBuilds() {
	// Build that was requested, and is still referenced by "events" (clients) or by
	// the build command that hasn't finished.
	// If the last "events" leaves, and "final" is set, we remove wipBuild.
	type wipBuild struct {
		// All listeners for events.
		events []chan buildUpdate

		// Last update, width done set to true. We store it to know the command has
		// finished, and give late arrivals the concluding update.
		final *buildUpdate
	}
	builds := map[request]*wipBuild{}

	active := 0
	maxBuilds := config.MaxBuilds
	if maxBuilds == 0 {
		maxBuilds = runtime.NumCPU() + 1
	}
	waiting := []request{}

	updatec := make(chan buildUpdate)

	startBuild := func(req request, b *wipBuild) {
		active++
		go func() {
			result, err := goBuild(req)
			var msg []byte
			if err == nil {
				msg = buildUpdateMsg{Kind: kindSuccess, Result: result}.json()
			} else if errors.Is(err, errTempFailure) {
				msg = buildUpdateMsg{Kind: kindTempFail, Error: err.Error()}.json()
			} else {
				msg = buildUpdateMsg{Kind: kindPermFail, Error: err.Error()}.json()
			}
			updatec <- buildUpdate{req: req, done: true, err: err, result: result, msg: msg}
		}()
	}

	intptr := func(i int) *int {
		return &i
	}

	sendPending := func(b *wipBuild, position int) {
		update := buildUpdate{
			queuePosition: position,
			msg:           buildUpdateMsg{Kind: kindQueuePosition, QueuePosition: intptr(position)}.json(),
		}
		for _, c := range b.events {
			select {
			case c <- update:
			default:
			}
		}
	}

	for {
		select {
		case br := <-coordinate.register:
			b, ok := builds[br.req]
			if !ok {
				b = &wipBuild{nil, nil}
				builds[br.req] = b
			}
			b.events = append(b.events, br.eventc)
			if b.final != nil {
				br.eventc <- *b.final
				continue
			}

			if !ok {
				if active < maxBuilds {
					startBuild(br.req, b)
				} else {
					waiting = append(waiting, br.req)
				}
			}
			update := buildUpdate{
				queuePosition: len(waiting),
				msg:           buildUpdateMsg{Kind: kindQueuePosition, QueuePosition: intptr(len(waiting))}.json(),
			}
			br.eventc <- update

		case br := <-coordinate.unregister:
			b := builds[br.req]
			l := []chan buildUpdate{}
			for _, c := range b.events {
				if c != br.eventc {
					l = append(l, c)
				}
			}
			if len(l) != len(b.events)-1 {
				panic("did not find channel")
			}
			b.events = l
			if len(b.events) == 0 && b.final != nil {
				delete(builds, br.req)
			}

		case update := <-updatec:
			b := builds[update.req]
			for _, c := range b.events {
				// We don't want to block. Slow clients/readers may not get all updates, better than blocking.
				select {
				case c <- update:
				default:
				}
			}
			if !update.done {
				continue
			}
			b.final = &update
			active--
			if len(b.events) == 0 {
				delete(builds, update.req)
			}
			for len(waiting) > 0 {
				req := waiting[0]
				waiting = waiting[1:]
				nb := builds[req]
				if len(nb.events) == 0 {
					// All parties interested have gone, don't build.
					continue
				}
				sendPending(nb, 0)
				startBuild(req, nb)
				for i, wreq := range waiting {
					sendPending(builds[wreq], i+1)
				}
				break
			}
		}
	}
}
