package usage

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxTranscriptLineBytes = 64 * 1024 * 1024

// readTranscriptLine limits the bytes before the newline, drains oversized
// lines, and returns an unterminated final line before reporting EOF.
func readTranscriptLine(reader *bufio.Reader, maxBytes int) ([]byte, bool, error) {
	var contents []byte
	oversized := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if err != nil && !errors.Is(err, bufio.ErrBufferFull) && !errors.Is(err, io.EOF) {
			return nil, false, fmt.Errorf("read transcript line: %w", err)
		}
		if len(fragment) > 0 && fragment[len(fragment)-1] == '\n' {
			fragment = fragment[:len(fragment)-1]
		}
		if !oversized {
			if len(fragment) > maxBytes-len(contents) {
				oversized = true
				contents = nil
			} else {
				contents = append(contents, fragment...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(contents) == 0 && !oversized {
			return nil, false, io.EOF
		}
		return contents, oversized, nil
	}
}

func openTranscriptFile(path string) (*os.File, error) {
	cleanPath := filepath.Clean(path)
	parent := filepath.Dir(cleanPath)
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, fmt.Errorf("open transcript parent %q: %w", parent, err)
	}
	file, openErr := root.Open(filepath.Base(cleanPath))
	if openErr != nil {
		closeErr := root.Close()
		if closeErr != nil {
			return nil, errors.Join(
				fmt.Errorf("open transcript %q: %w", cleanPath, openErr),
				fmt.Errorf("close transcript parent %q: %w", parent, closeErr),
			)
		}
		return nil, fmt.Errorf("open transcript %q: %w", cleanPath, openErr)
	}
	if closeErr := root.Close(); closeErr != nil {
		fileCloseErr := file.Close()
		if fileCloseErr != nil {
			return nil, errors.Join(
				fmt.Errorf("close transcript parent %q: %w", parent, closeErr),
				fmt.Errorf("close transcript %q: %w", cleanPath, fileCloseErr),
			)
		}
		return nil, fmt.Errorf("close transcript parent %q: %w", parent, closeErr)
	}
	return file, nil
}
