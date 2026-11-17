package main

// storesCleanup keeps diagnostic-worker joining separate from closing the shared
// execution pool. An unjoined optional reader may retain only that pool until
// fatal process exit; it cannot skip unrelated child cleanup.
type storesCleanup struct {
	closeWorker func()
	closePool   func()
	retainPool  bool
}

func (s *storesCleanup) RetainPoolUntilExit() { s.retainPool = true }

func (s *storesCleanup) Close() {
	if s.closeWorker != nil {
		s.closeWorker()
	}
	if !s.retainPool && s.closePool != nil {
		s.closePool()
	}
}
