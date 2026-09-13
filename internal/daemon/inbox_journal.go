package daemon

import (
	"fmt"
	"os"

	"github.com/agynio/agynd-cli/internal/inboxjournal"
)

type terminalInboxError struct{ err error }

func (e *terminalInboxError) Error() string { return e.err.Error() }
func (e *terminalInboxError) Unwrap() error { return e.err }

func (d *Daemon) messageJournal() (*inboxjournal.Journal, error) {
	if d.inboxJournalReady {
		return d.inboxJournal, nil
	}
	directory := os.Getenv("AGYN_INBOX_JOURNAL_DIR")
	control := os.Getenv("AGYN_INBOX_CONTROL_FILE")
	if directory == "" {
		if control != "" {
			return nil, fmt.Errorf("inbox control requires a durable inbox journal")
		}
		d.inboxJournalReady = true
		return nil, nil
	}
	journal, err := inboxjournal.Open(directory, d.cfg.AgentInstanceID.String(), control)
	if err != nil {
		return nil, err
	}
	d.inboxJournal = journal
	d.inboxJournalReady = true
	return journal, nil
}
