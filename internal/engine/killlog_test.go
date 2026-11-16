package engine

// kill releases the log the way a killed process does: no final work beyond
// stopping the background syncer, which must not outlive the file.
func (l *logFile) kill() {
	if l.bg != nil {
		_ = l.bg.stop()
		l.bg = nil
	}
	l.f.Close()
}
