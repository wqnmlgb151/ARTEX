package server

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/google/uuid"
)

func mergedTimeoutIDs(items []triggeredRun) (ids []int64) {
	for _, it := range items {
		ids = append(ids, it.timeoutDeliveryIDs...)
	}
	return
}
func mergedTimeoutKeys(items []triggeredRun) (keys []string) {
	for _, it := range items {
		keys = append(keys, it.timeoutQueueKeys...)
	}
	return
}

func (s *Server) enqueueTimeoutRun(item triggeredRun, key string) {
	cfg := s.readTriggerBehavior(item.agentKey)
	s.queueMu.Lock()
	defer s.queueMu.Unlock()
	if s.timeoutQueued == nil {
		s.timeoutQueued = map[string]bool{}
	}
	if s.timeoutQueued[key] {
		return
	}
	s.timeoutQueued[key] = true
	item.timeoutQueueKeys = []string{key}
	s.triggerCfg[item.agentKey] = cfg
	s.triggerQ[item.agentKey] = append(s.triggerQ[item.agentKey], item)
	s.pumpLocked(item.agentKey)
}

func (s *Server) runDurableTimeout(item triggeredRun) {
	owner := uuid.NewString()
	pg := s.m.pg
	msg := finalTriggerMessage(item)
	c, id, err := pg.ClaimTimeoutRun(item.timeoutDeliveryIDs, db.TimeoutRun{ID: item.timeoutRunID, AgentKey: item.agentKey, Title: firstLine(item.title, 60), Message: msg}, owner)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("[trigger] timeout claim failed: %v", err)
		}
		return
	}
	// External tool side effects are at-least-once after a crash. The durable
	// conversation and human message are created once and reused on every retry.
	s.chatMu.Lock()
	s.chatBusy[s.convBusyKey(c.ID)] = true
	s.chatMu.Unlock()
	ctx, cancel := s.conversationRunContext(c.ID, s.convBusyKey(c.ID))
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := pg.RenewTimeoutRun(id, owner); err != nil {
					log.Printf("[trigger] timeout run %d renewal failed: %v", id, err)
					cancel(err)
					return
				}
			}
		}
	}()
	var runErr error
	func() {
		defer func() {
			if r := recover(); r != nil {
				runErr = fmt.Errorf("trigger run panic: %v", r)
			}
		}()
		runErr = s.runConversationTurn(ctx, cancel, c, msg, s.convBusyKey(c.ID))
	}()
	cancel(runErr)
	<-done
	if err := pg.FinishTimeoutRun(id, owner, runErr); err != nil {
		log.Printf("[trigger] timeout run %d acknowledgement failed: %v", id, err)
	}
	if runErr != nil {
		log.Printf("[trigger] timeout run %d failed; retry scheduled: %v", id, runErr)
	}
}
