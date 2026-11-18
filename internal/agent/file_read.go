package agent

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
)

// Hash the complete source with bounded memory, then gate the same open file
// before selecting a page. Offset/line/base64 requests cannot bypass the gate.
func readFilePage(path string, r FileRequest, checks []func(string) error) (any, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("read requires a regular file")
	}
	if r.Offset < 0 || r.Limit < 0 || int64(r.Offset) > info.Size() {
		return nil, fmt.Errorf("invalid byte range")
	}
	if r.StartLine < 0 || r.LineCount < 0 {
		return nil, fmt.Errorf("invalid line range")
	}
	digest := sha256.New()
	n, err := io.Copy(digest, f)
	if err != nil {
		return nil, err
	}
	hash := hex.EncodeToString(digest.Sum(nil))
	for _, check := range checks {
		if err = check(hash); err != nil {
			return nil, err
		}
	}
	limit := r.Limit
	if limit == 0 {
		limit = 16 << 10
	}
	limit = min(limit, 64<<10)
	start := int64(r.Offset)
	if r.StartLine > 0 {
		start = 0
	}
	if _, err = f.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	var page []byte
	if r.StartLine > 0 {
		reader := bufio.NewReaderSize(f, 32<<10)
		for line := 1; line < r.StartLine; {
			part, readErr := reader.ReadSlice('\n')
			start += int64(len(part))
			if readErr == bufio.ErrBufferFull {
				continue
			}
			if readErr == io.EOF {
				return nil, fmt.Errorf("start_line exceeds file")
			}
			if readErr != nil {
				return nil, readErr
			}
			line++
		}
		count := r.LineCount
		if count == 0 {
			count = 200
		}
		for line := 0; line < count && len(page) < 64<<10; {
			part, readErr := reader.ReadSlice('\n')
			page = append(page, part[:min(len(part), (64<<10)-len(page))]...)
			if readErr == io.EOF {
				break
			}
			if readErr == bufio.ErrBufferFull {
				continue
			}
			if readErr != nil {
				return nil, readErr
			}
			line++
		}
	} else {
		page, err = io.ReadAll(io.LimitReader(f, int64(limit)))
		if err != nil {
			return nil, err
		}
	}
	end := start + int64(len(page))
	header := make([]byte, 512)
	headerBytes, err := f.ReadAt(header, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	result := map[string]any{"path": path, "hash": hash, "bytes": int(info.Size()), "offset": int(start), "next_offset": int(end), "content": string(page), "data_base64": base64.StdEncoding.EncodeToString(page), "mime_type": http.DetectContentType(header[:headerBytes]), "eof": end == info.Size()}
	if config, format, err := image.DecodeConfig(io.NewSectionReader(f, 0, 64<<10)); err == nil {
		result["image"] = map[string]any{"format": format, "width": config.Width, "height": config.Height}
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if n != info.Size() || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, fmt.Errorf("file changed during read")
	}
	if r.StartLine > 0 {
		result["start_line"] = r.StartLine
		result["next_line"] = r.StartLine + bytes.Count(page, []byte("\n"))
	}
	return result, nil
}
