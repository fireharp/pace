//go:build !darwin && !linux

package daemon

// getPeerCred: no peer credentials on this OS; the 0700 socket directory is
// the only protection, so the peer is reported as unknown (allowed).
func getPeerCred(fd int) (peerCred, error) { return peerCred{}, nil }
