package server

// ReleaseWaiters answers every parked and future /wait long-poll with 204 at
// once, so a shutdown doesn't hold them for the full poll timeout. The
// sender just re-polls, reaching the next process.
func (s *Server) ReleaseWaiters() {
	s.stopOnce.Do(func() { close(s.stopping) })
}
