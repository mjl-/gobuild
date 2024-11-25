package main

import (
	"context"
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

// buildUpdateMsg is sent to browsers through the SSE /events endpoint.
type buildUpdateMsg struct {
	Kind          kind
	QueuePosition *int         `json:",omitempty"`
	Error         string       `json:",omitempty"`
	Result        *buildResult `json:",omitempty"`
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
	bs            buildSpec
	done          bool         // If true, build finished, failure or success.
	err           error        // If not nil, build failed.
	result        *buildResult // Only in case of success.
	recordNumber  int64        // Only in case of success.
	queuePosition int          // If 0, no longer queued, but building.
	msg           []byte       // JSON-encoded buildUpdateMsg to write to clients.
}

type buildRequest struct {
	bs     buildSpec
	expSum string // If non-empty, build must result in this sum. Used for rebuilding a binary that was cleaned up.
	eventc chan buildUpdate
}

var coordinate = struct {
	register   chan buildRequest
	unregister chan buildRequest
}{
	make(chan buildRequest, 1),
	make(chan buildRequest, 1),
}

func registerBuild(bs buildSpec, expSum string, eventc chan buildUpdate) {
	coordinate.register <- buildRequest{bs, expSum, eventc}
}

func unregisterBuild(bs buildSpec, eventc chan buildUpdate) {
	coordinate.unregister <- buildRequest{bs, "", eventc}
}

func coordinateBuilds() {
	// Build that was requested, and is still referenced by "events" (clients) or by
	// the build command that hasn't finished.
	// If the last "events" leaves, and "final" is set, we remove wipBuild.
	type wipBuild struct {
		// All listeners for events. These will all get updates.
		events []chan buildUpdate

		// Last update, with done set to true. We store it to know the command has
		// finished, and give all listeners the concluding update.
		final *buildUpdate
	}
	builds := map[buildSpec]*wipBuild{}

	active := 0
	maxBuilds := config.MaxBuilds
	if maxBuilds == 0 {
		maxBuilds = runtime.NumCPU() + 1
	}

	// Build requests always go through the queue. We'll pick up the next for which the
	// output path is available, but only if we are below maxBuilds builds in progress.
	queue := []buildRequest{}

	// Keep track of output paths that are "busy", i.e. paths that currently running
	// builds will write the resulting binary to.
	// Keys are the result of request.outputPath.
	pathBusy := map[string]struct{}{}

	updatec := make(chan buildUpdate)

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

	startBuild := func(breq buildRequest, b *wipBuild) {
		active++
		pathBusy[breq.bs.outputPath()] = struct{}{}
		go func() {
			recordNumber, result, errOutput, err := build(breq.bs, breq.expSum)
			var errmsg string
			if err != nil {
				errmsg = err.Error() + "\n\n" + errOutput
			}
			var msg []byte
			if err == nil {
				msg = buildUpdateMsg{Kind: kindSuccess, Result: result}.json()
			} else if errors.Is(err, errTempFailure) {
				msg = buildUpdateMsg{Kind: kindTempFail, Error: errmsg}.json()
			} else {
				msg = buildUpdateMsg{Kind: kindPermFail, Error: errmsg}.json()
			}
			update := buildUpdate{bs: breq.bs, done: true, err: err, result: result, recordNumber: recordNumber, msg: msg}
			updatec <- update

			// Once every 20 builds, clear the build cache, to prevent the disk from filling up too easily.
			if err == nil && recordNumber%20 == 0 {
				cleanupGoBuildCache()
			}
		}()
	}

	kick := func() {
		if active >= maxBuilds {
			return
		}

		for i := 0; i < len(queue); {
			breq := queue[i]
			if _, busy := pathBusy[breq.bs.outputPath()]; busy {
				i++
				continue
			}
			queue = append(queue[:i], queue[i+1:]...)
			nb := builds[breq.bs]
			if len(nb.events) == 0 {
				// All parties interested have gone, don't build.
				continue
			}
			sendPending(nb, 0)
			startBuild(breq, nb)
			for j, wbreq := range queue[i:] {
				sendPending(builds[wbreq.bs], i+j+1)
			}
			break
		}
	}

	for {
		select {
		case reg := <-coordinate.register:
			b, ok := builds[reg.bs]
			if !ok {
				b = &wipBuild{nil, nil}
				builds[reg.bs] = b

				// We may have just finished a build. Before starting any new work, try reading a result.
				if recordNumber, br, binaryPresent, failed, err := (serverOps{}.lookupResult(context.Background(), reg.bs)); err != nil || failed {
					if err == nil {
						err = fmt.Errorf("build failed")
					}
					msg := buildUpdateMsg{Kind: kindTempFail, Error: err.Error()}.json()
					b.final = &buildUpdate{reg.bs, true, err, nil, 0, 0, msg}
				} else if br != nil && binaryPresent {
					msg := buildUpdateMsg{Kind: kindSuccess, Result: br}.json()
					b.final = &buildUpdate{reg.bs, true, nil, br, recordNumber, 0, msg}
				}
				// Else no result or no binary, we'll continue as normal, starting a build.
			}
			b.events = append(b.events, reg.eventc)
			if b.final != nil {
				reg.eventc <- *b.final
				continue
			}

			if !ok {
				queue = append(queue, reg)
				kick()
			}
			update := buildUpdate{
				queuePosition: len(queue),
				msg:           buildUpdateMsg{Kind: kindQueuePosition, QueuePosition: intptr(len(queue))}.json(),
			}
			reg.eventc <- update

		case reg := <-coordinate.unregister:
			b := builds[reg.bs]
			l := []chan buildUpdate{}
			for _, c := range b.events {
				if c != reg.eventc {
					l = append(l, c)
				}
			}
			if len(l) != len(b.events)-1 {
				panic("did not find channel")
			}
			b.events = l
			if len(b.events) == 0 && b.final != nil {
				delete(builds, reg.bs)
			}

		case update := <-updatec:
			b := builds[update.bs]
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
			delete(pathBusy, update.bs.outputPath())
			b.final = &update
			active--
			if len(b.events) == 0 {
				delete(builds, update.bs)
			}
			kick()
		}
	}
}
