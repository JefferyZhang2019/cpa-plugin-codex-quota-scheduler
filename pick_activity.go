package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type pickActivityIntent struct {
	Request pluginapi.SchedulerPickRequest
	Version uint64
	Now     time.Time
}

type pickObservationIntent struct {
	Request  pluginapi.SchedulerPickRequest
	Decision PickDecision
	Now      time.Time
}

type pickActivityPump struct {
	latest           atomic.Pointer[pickActivityIntent]
	wake             chan struct{}
	ctx              context.Context
	cancel           context.CancelFunc
	done             chan struct{}
	observations     chan pickObservationIntent
	observationDone  chan struct{}
	observationDrops atomic.Uint64
	stopped          atomic.Bool
	stopOnce         sync.Once
	roster           *RosterController
	refresher        *QuotaRefresher
	state            *PluginState
}

func newPickActivityPump(roster *RosterController, refresher *QuotaRefresher, state *PluginState) *pickActivityPump {
	ctx, cancel := context.WithCancel(context.Background())
	p := &pickActivityPump{
		wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel, done: make(chan struct{}),
		observations: make(chan pickObservationIntent, 256), observationDone: make(chan struct{}),
		roster: roster, refresher: refresher, state: state,
	}
	go p.run()
	go p.runObservations()
	return p
}

func (p *pickActivityPump) enqueue(req pluginapi.SchedulerPickRequest, version uint64, now time.Time) {
	if p == nil || p.stopped.Load() {
		return
	}
	copy := req
	copy.Candidates = append([]pluginapi.SchedulerAuthCandidate(nil), req.Candidates...)
	copy.Providers = append([]string(nil), req.Providers...)
	p.latest.Store(&pickActivityIntent{Request: copy, Version: version, Now: now})
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *pickActivityPump) enqueueObservation(req pluginapi.SchedulerPickRequest, decision PickDecision, now time.Time) {
	if p == nil || p.stopped.Load() {
		return
	}
	copy := req
	copy.Candidates = append([]pluginapi.SchedulerAuthCandidate(nil), req.Candidates...)
	copy.Providers = append([]string(nil), req.Providers...)
	select {
	case p.observations <- pickObservationIntent{Request: copy, Decision: decision, Now: now}:
	default:
		p.observationDrops.Add(1)
	}
}

func (p *pickActivityPump) runObservations() {
	defer close(p.observationDone)
	for {
		select {
		case <-p.ctx.Done():
			return
		case intent := <-p.observations:
			if p.state == nil {
				continue
			}
			p.state.RecordCodexActivity(intent.Now)
			if intent.Decision.AuthID != "" {
				p.state.RecordSelection(intent.Decision.AuthID, intent.Decision.Reason)
			}
			logSchedulerDecision(p.state, intent.Request, intent.Decision, intent.Now)
		}
	}
}

func (p *pickActivityPump) run() {
	defer close(p.done)
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.wake:
			for {
				intent := p.latest.Swap(nil)
				if intent == nil {
					break
				}
				if p.roster != nil {
					_, _ = p.roster.WakeForActivity(p.ctx)
				}
				if p.ctx.Err() != nil {
					return
				}
				if p.refresher != nil {
					p.refresher.OnSchedulerPick(intent.Request, intent.Version, intent.Now)
				}
			}
		}
	}
}

func (p *pickActivityPump) stop() {
	if p == nil {
		return
	}
	p.stopOnce.Do(func() {
		p.stopped.Store(true)
		p.cancel()
	})
}

var globalPickActivityPump atomic.Pointer[pickActivityPump]

func replaceGlobalPickActivityPump(roster *RosterController, refresher *QuotaRefresher) {
	next := newPickActivityPump(roster, refresher, globalState)
	previous := globalPickActivityPump.Swap(next)
	if previous != nil {
		previous.stop()
	}
}

func stopGlobalPickActivityPump() {
	if p := globalPickActivityPump.Swap(nil); p != nil {
		p.stop()
	}
}
