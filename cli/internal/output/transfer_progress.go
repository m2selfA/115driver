package output

import (
	"os"
	"strings"
	"sync"

	"github.com/cheggaaa/pb/v3"
)

type TransferProgress struct {
	mu      sync.Mutex
	enabled bool
	bar     *pb.ProgressBar
	status  string
}

func NewTransferProgress() *TransferProgress {
	return &TransferProgress{enabled: terminalFile(os.Stderr)}
}

func (progress *TransferProgress) Enabled() bool {
	return progress != nil && progress.enabled
}

func (progress *TransferProgress) SetStatus(status string) {
	if progress == nil {
		return
	}
	progress.mu.Lock()
	defer progress.mu.Unlock()
	progress.status = compactTransferProgressStatus(status)
	if progress.bar != nil {
		progress.bar.Set("status", progress.status)
	}
}

func (progress *TransferProgress) SetProgress(completed, total int64) {
	if progress == nil || !progress.enabled || total < 0 {
		return
	}
	if completed < 0 {
		completed = 0
	}
	if completed > total {
		completed = total
	}
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.bar == nil {
		progress.bar = pb.New64(total)
		progress.bar.SetWriter(os.Stderr)
		progress.bar.SetTemplateString(`{{string . "status"}} {{counters . }} {{bar . }} {{percent . }} {{speed . }}`)
		progress.bar.Set("status", progress.status)
		progress.bar.Start()
	}
	progress.bar.SetCurrent(completed)
}

func (progress *TransferProgress) Finish() {
	if progress == nil {
		return
	}
	progress.mu.Lock()
	defer progress.mu.Unlock()
	if progress.bar != nil {
		progress.bar.Finish()
		progress.bar = nil
	}
}

func terminalFile(file *os.File) bool {
	if file == nil {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func compactTransferProgressStatus(status string) string {
	status = strings.Join(strings.Fields(status), " ")
	const maxRunes = 72
	runes := []rune(status)
	if len(runes) <= maxRunes {
		return status
	}
	return string(runes[:maxRunes-1]) + "…"
}
