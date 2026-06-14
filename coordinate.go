package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/netip"
	"runtime"
	"slices"
	"time"
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
	return fmt.Appendf(nil, "event: update\ndata: %s\n\n", event)
}

type buildUpdate struct {
	bs            buildSpec
	done          bool         // If true, build finished, failure or success.
	err           error        // If not nil, build failed.
	errOutput     string       // Output of build, if it failed during a build command.
	noBuildReason string       // If the build failed, and it won't succeed in the future, this is a short reason.
	result        *buildResult // Only in case of success.
	recordNumber  int64        // Only in case of success.
	queuePosition int          // If 0, no longer queued, but building.
	msg           []byte       // JSON-encoded buildUpdateMsg to write to clients.
}

type buildRequest struct {
	bs     buildSpec
	expSum string // If non-empty, build must result in this sum. Used for rebuilding a binary that was cleaned up.
	eventc chan buildUpdate
	remote netip.Addr
	log    *slog.Logger
}

type buildUnregister struct {
	bs     buildSpec
	eventc chan buildUpdate
}

type coordinatorState struct {
	builds map[buildSpec]*wipBuild
}

var coordinate = struct {
	register   chan buildRequest
	unregister chan buildUnregister
	state      chan chan coordinatorState
}{
	make(chan buildRequest),
	make(chan buildUnregister),
	make(chan chan coordinatorState),
}

func registerBuild(log *slog.Logger, bs buildSpec, expSum string, eventc chan buildUpdate, remote netip.Addr) {
	coordinate.register <- buildRequest{bs, expSum, eventc, remote, log}
}

func unregisterBuild(bs buildSpec, eventc chan buildUpdate) {
	coordinate.unregister <- buildUnregister{bs, eventc}
}

// Build that was requested, and is still referenced by "events" (clients) or by
// the build command that hasn't finished.
// If the last "events" leaves, and "final" is set, we remove wipBuild.
type wipBuild struct {
	// All listeners for events. These will all get updates.
	events []chan buildUpdate

	added   time.Time
	started *time.Time

	// Last update, with done set to true. We store it to know the command has
	// finished, and give all listeners the concluding update.
	final *buildUpdate

	initiator netip.Addr
}

func coordinateBuilds(ctx context.Context) {
	builds := map[buildSpec]*wipBuild{}

	active := 0
	maxBuilds := config.MaxBuilds
	if maxBuilds == 0 {
		maxBuilds = runtime.NumCPU() + 1
	}

	// Build requests always go through the queue. We'll pick up the next for which the
	// output path is available, but only if we are below maxBuilds builds in progress.
	queue := []buildRequest{}

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
		now := time.Now()
		b.started = &now
		wgShutdown.Go(func() {
			defer logPanic(breq.log)

			breq.log.Info("starting build", "buildspec", breq.bs)

			metricBuildTotal.Inc()
			bctx := context.WithValue(ctx, ctxKeyLog{}, breq.log)
			recordNumber, result, errOutput, noBuildReason, err := build(bctx, breq.bs, breq.expSum)
			var errmsg string
			if err != nil {
				errmsg = err.Error() + "\n\n" + errOutput
				metricBuildErrors.Inc()
				if errOutput != "" && noBuildReason == "" {
					metricBuildErrorsUnknownReason.Inc()
				}
			}
			var msg []byte
			if err == nil {
				msg = buildUpdateMsg{Kind: kindSuccess, Result: result}.json()
			} else if errors.Is(err, errTempFailure) || errors.Is(err, context.Canceled) {
				msg = buildUpdateMsg{Kind: kindTempFail, Error: errmsg}.json()
			} else {
				msg = buildUpdateMsg{Kind: kindPermFail, Error: errmsg}.json()
			}
			update := buildUpdate{bs: breq.bs, done: true, err: err, errOutput: errOutput, noBuildReason: noBuildReason, result: result, recordNumber: recordNumber, msg: msg}
			updatec <- update
		})
	}

	kick := func() {
		if active >= maxBuilds {
			return
		}

		for len(queue) > 0 {
			breq := queue[0]
			queue = slices.Delete(queue, 0, 1)
			nb := builds[breq.bs]
			if len(nb.events) == 0 {
				// All parties interested have gone, don't build.
				continue
			}
			sendPending(nb, 0)
			startBuild(breq, nb)
			for j, wbreq := range queue {
				sendPending(builds[wbreq.bs], j+1)
			}
			break
		}
	}

	for {
		select {
		case reg := <-coordinate.register:
			b, ok := builds[reg.bs]
			if !ok {
				b = &wipBuild{nil, time.Now(), nil, nil, reg.remote}
				builds[reg.bs] = b

				// We may have just finished a build. Before starting any new work, try reading a result.
				if recordNumber, br, binaryPresent, failed, err := (serverOps{}.lookupResult(ctx, reg.bs)); err != nil || failed {
					if err == nil {
						err = fmt.Errorf("build failed")
					}
					msg := buildUpdateMsg{Kind: kindTempFail, Error: err.Error()}.json()
					b.final = &buildUpdate{reg.bs, true, err, "", "", nil, 0, 0, msg}
				} else if br != nil && binaryPresent {
					msg := buildUpdateMsg{Kind: kindSuccess, Result: br}.json()
					b.final = &buildUpdate{reg.bs, true, nil, "", "", br, recordNumber, 0, msg}
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
				queuePosition: slices.IndexFunc(queue, func(e buildRequest) bool {
					return e.bs == reg.bs
				}),
				msg: buildUpdateMsg{Kind: kindQueuePosition, QueuePosition: intptr(len(queue))}.json(),
			}
			reg.eventc <- update

		case unreg := <-coordinate.unregister:
			b := builds[unreg.bs]
			l := []chan buildUpdate{}
			for _, c := range b.events {
				if c != unreg.eventc {
					l = append(l, c)
				}
			}
			if len(l) != len(b.events)-1 {
				panic("did not find channel")
			}
			b.events = l
			if len(b.events) == 0 && b.final != nil {
				delete(builds, unreg.bs)
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
			b.final = &update
			active--
			if len(b.events) == 0 {
				delete(builds, update.bs)
			}
			kick()

		case rc := <-coordinate.state:
			rc <- coordinatorState{maps.Clone(builds)}
		}
	}
}
