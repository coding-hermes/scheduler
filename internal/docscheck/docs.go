// Package docscheck guards the repository's operating documents: the artefacts a
// lane reads when it does not know what to do next. They are not prose for its
// own sake — if the routing table rots, lanes go back to waiting for a human,
// which is the failure mode the table exists to prevent.
package docscheck
