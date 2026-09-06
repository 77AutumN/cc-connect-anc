package feishu

// Register every accepted message before asynchronous I/O. Reuse the image
// batch's completion chain so post/quote downloads and later text cannot race
// the Engine watermark. No model wait, polling or new persistent queue.
func (p *Platform) queueMessageDispatch(sessionKey string, dispatch func()) {
	p.imageBatchMu.Lock()
	pending := p.imageBatch[sessionKey]
	if pending != nil {
		if pending.timer != nil {
			pending.timer.Stop()
		}
		delete(p.imageBatch, sessionKey)
	}
	if p.imageBatchTail == nil {
		p.imageBatchTail = make(map[string]*imageBatchEntry)
	}
	turn := &imageBatchEntry{sessionKey: sessionKey, done: make(chan struct{})}
	if previous := p.imageBatchTail[sessionKey]; previous != nil {
		turn.previousDone = previous.done
	}
	p.imageBatchTail[sessionKey] = turn
	p.imageBatchMu.Unlock()
	go func() {
		defer p.finishImageDispatch(turn)
		if pending != nil {
			p.dispatchImageBatchEntry(pending)
		}
		if turn.previousDone != nil {
			<-turn.previousDone
		}
		dispatch()
	}()
}

func (p *Platform) finishImageDispatch(entry *imageBatchEntry) {
	p.imageBatchMu.Lock()
	defer p.imageBatchMu.Unlock()
	if p.imageBatchTail[entry.sessionKey] == entry {
		delete(p.imageBatchTail, entry.sessionKey)
	}
	close(entry.done)
}
