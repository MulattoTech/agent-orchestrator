package httpd

// Close releases listeners even if construction of a later daemon dependency
// failed before Run was entered. Run remains the owner of graceful HTTP drain
// and its run-file; calling Close after Run is harmless.
func (s *Server) Close() {
	if s == nil {
		return
	}
	_ = s.http.Close()
	_ = s.listen.Close()
	if s.listen6 != nil {
		_ = s.listen6.Close()
	}
}
