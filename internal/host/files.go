package host

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/soren-labs/hostmcp/internal/protocol"
)

type ReadInput struct {
	Paths     []string `json:"paths"`
	Cwd       string   `json:"cwd,omitempty"`
	StartLine int      `json:"start_line,omitempty"`
	EndLine   int      `json:"end_line,omitempty"`
	MaxBytes  *int     `json:"max_bytes,omitempty"`
	Encoding  string   `json:"encoding,omitempty"`
}
type WriteInput struct {
	Path          string `json:"path"`
	Content       string `json:"content"`
	Cwd           string `json:"cwd,omitempty"`
	Encoding      string `json:"encoding,omitempty"`
	Overwrite     bool   `json:"overwrite,omitempty"`
	CreateParents bool   `json:"create_parents,omitempty"`
	Mode          string `json:"mode,omitempty"`
}
type ImageInput struct {
	Path string `json:"path"`
	Cwd  string `json:"cwd,omitempty"`
}

// A FIFO can otherwise block in open before any f.Stat or context cancellation.
// Check the path first, then open nonblocking and check the actual descriptor too,
// so replacement between Stat and Open cannot turn an ordinary file tool into a
// blocking special-file operation. Defer truncation until descriptor validation.
func openRegular(path string, flags int, mode os.FileMode) (*os.File, error) {
	if info, err := os.Stat(path); err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%s is not a regular file; use an explicit shell command for special files", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) || flags&os.O_CREATE == 0 {
		return nil, err
	}
	truncate := flags&os.O_TRUNC != 0
	flags &^= os.O_TRUNC
	fd, err := syscall.Open(path, flags|syscall.O_NONBLOCK|syscall.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s changed to a non-regular file while opening", path)
	}
	if truncate {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return nil, err
		}
	}
	return f, nil
}

func openDirectory(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.IsDir() {
		f.Close()
		return nil, fmt.Errorf("%s changed to a non-directory while opening", path)
	}
	return f, nil
}

func (r *Runtime) read(in ReadInput) (protocol.Result, error) {
	if len(in.Paths) == 0 || len(in.Paths) > 16 {
		return protocol.Result{}, errors.New("paths must contain between 1 and 16 entries")
	}
	budget, err := bounds(in.MaxBytes, 65536, 1024, maxFileBytes, "max_bytes")
	if err != nil {
		return protocol.Result{}, err
	}
	if in.StartLine == 0 {
		in.StartLine = 1
	}
	if in.StartLine < 1 || in.StartLine > 10000000 || in.EndLine < 0 || in.EndLine > 10000000 || in.EndLine > 0 && in.EndLine < in.StartLine {
		return protocol.Result{}, errors.New("invalid text line range")
	}
	if in.Encoding == "" {
		in.Encoding = "utf8"
	}
	if in.Encoding != "utf8" && in.Encoding != "base64" {
		return protocol.Result{}, errors.New("encoding must be utf8 or base64")
	}
	entries := make([]map[string]any, 0, len(in.Paths))
	for _, value := range in.Paths {
		path, e := r.path(value, in.Cwd)
		if e != nil {
			return protocol.Result{}, e
		}
		entry, used, e := readPath(path, in, budget)
		if e != nil {
			if len(in.Paths) == 1 {
				return protocol.Result{}, e
			}
			entry = map[string]any{"path": path, "error": e.Error()}
		}
		entries = append(entries, entry)
		budget -= used
		if budget < 0 {
			budget = 0
		}
	}
	result := protocol.JSONResult(map[string]any{"entries": entries})
	if len(result.Data) > protocol.MaxMessageBytes-65536 {
		return protocol.Result{}, errors.New("encoded read result exceeds the transport limit; reduce max_bytes or use encoding=base64")
	}
	return result, nil
}

func readPath(path string, in ReadInput, budget int) (map[string]any, int, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	entry := map[string]any{"path": path, "size": info.Size(), "mode": info.Mode().String(), "modified_at": info.ModTime().UTC()}
	if info.IsDir() {
		entry["type"] = "directory"
		f, err := openDirectory(path)
		if err != nil {
			return nil, 0, err
		}
		defer f.Close()
		children, err := f.ReadDir(1001)
		if err != nil && err != io.EOF {
			return nil, 0, err
		}
		truncated := len(children) > 1000
		if truncated {
			children = children[:1000]
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		items := make([]map[string]any, 0, len(children))
		used := 0
		for _, child := range children {
			cost := len(child.Name()) + 80
			if used+cost > budget {
				truncated = true
				break
			}
			used += cost
			items = append(items, map[string]any{"name": child.Name(), "type": child.Type().String(), "directory": child.IsDir()})
		}
		entry["entries"] = items
		entry["truncated"] = truncated
		return entry, used, nil
	}
	if !info.Mode().IsRegular() {
		entry["type"] = "special"
		entry["note"] = "Only metadata is returned for special files; use an explicit shell command when needed."
		return entry, 0, nil
	}
	entry["type"] = "file"
	if budget == 0 {
		entry["truncated"] = info.Size() > 0
		entry["note"] = "Combined read budget exhausted"
		return entry, 0, nil
	}
	f, err := openRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	if in.Encoding == "base64" {
		b, err := io.ReadAll(io.LimitReader(f, int64(budget)+1))
		if err != nil {
			return nil, 0, err
		}
		truncated := len(b) > budget
		if truncated {
			b = b[:budget]
		}
		entry["encoding"] = "base64"
		entry["content"] = base64.StdEncoding.EncodeToString(b)
		entry["truncated"] = truncated
		return entry, len(b), nil
	}
	// ReadSlice bounds memory even if a malicious/generated file has a gigantic line.
	const scanLimit = 16 << 20
	reader := bufio.NewReaderSize(io.LimitReader(f, scanLimit+1), 32768)
	var result bytes.Buffer
	line, scanned := 1, 0
	lastLine := in.StartLine - 1
	truncated := false
	binary := false
	done := false
	for !done {
		part, e := reader.ReadSlice('\n')
		scanned += len(part)
		if scanned > scanLimit {
			truncated = true
			break
		}
		if bytes.IndexByte(part, 0) >= 0 {
			binary = true
			break
		}
		if line >= in.StartLine && (in.EndLine == 0 || line <= in.EndLine) {
			remaining := budget - result.Len()
			if len(part) > remaining {
				result.Write(part[:remaining])
				truncated = true
				break
			}
			result.Write(part)
			if len(part) > 0 {
				lastLine = line
			}
		}
		if len(part) > 0 && part[len(part)-1] == '\n' {
			if in.EndLine > 0 && line >= in.EndLine {
				done = true
			}
			line++
		}
		if result.Len() >= budget {
			if int64(scanned) < info.Size() {
				truncated = true
			}
			break
		}
		if e == io.EOF {
			break
		}
		if e != nil && e != bufio.ErrBufferFull {
			return nil, 0, e
		}
	}
	if binary {
		entry["binary"] = true
		entry["note"] = "NUL bytes detected; use encoding=base64 for binary content"
		entry["truncated"] = false
		return entry, 0, nil
	}
	data := result.Bytes()
	if truncated {
		data = data[:completeUTF8Prefix(data)]
	}
	entry["encoding"] = "utf8"
	entry["content"] = strings.ToValidUTF8(string(data), "\uFFFD")
	entry["start_line"] = in.StartLine
	entry["end_line"] = lastLine
	entry["truncated"] = truncated
	entry["scanned_bytes"] = scanned
	entry["invalid_utf8_replaced"] = !utf8.Valid(data)
	return entry, len(data), nil
}

func (r *Runtime) writeFile(in WriteInput) (protocol.Result, error) {
	path, err := r.path(in.Path, in.Cwd)
	if err != nil {
		return protocol.Result{}, err
	}
	var content []byte
	switch in.Encoding {
	case "", "utf8":
		content = []byte(in.Content)
	case "base64":
		content, err = base64.StdEncoding.DecodeString(in.Content)
		if err != nil {
			return protocol.Result{}, fmt.Errorf("invalid base64 content: %w", err)
		}
	default:
		return protocol.Result{}, errors.New("encoding must be utf8 or base64")
	}
	if len(content) > maxFileBytes {
		return protocol.Result{}, errors.New("decoded file content exceeds 1 MiB")
	}
	mode := os.FileMode(0644)
	if in.Mode != "" {
		n, e := strconv.ParseUint(in.Mode, 8, 32)
		if e != nil || n > 0777 {
			return protocol.Result{}, errors.New("mode must be octal permissions between 0000 and 0777")
		}
		mode = os.FileMode(n)
	}
	if in.CreateParents {
		if err = os.MkdirAll(nativeDir(path), 0755); err != nil {
			return protocol.Result{}, err
		}
	}
	flag := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if in.Overwrite {
		flag = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := openRegular(path, flag, mode)
	if err != nil {
		return protocol.Result{}, err
	}
	n, writeErr := f.Write(content)
	closeErr := f.Close()
	if writeErr != nil {
		return protocol.Result{}, fmt.Errorf("write %s failed after %d bytes: %w", path, n, writeErr)
	}
	if closeErr != nil {
		return protocol.Result{}, fmt.Errorf("close %s after writing %d bytes: %w", path, n, closeErr)
	}
	return protocol.JSONResult(map[string]any{"path": path, "written_bytes": n, "sha256": fmt.Sprintf("%x", sha256.Sum256(content))}), nil
}

func (r *Runtime) viewImage(in ImageInput) (protocol.Result, error) {
	path, err := r.path(in.Path, in.Cwd)
	if err != nil {
		return protocol.Result{}, err
	}
	f, err := openRegular(path, os.O_RDONLY, 0)
	if err != nil {
		return protocol.Result{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return protocol.Result{}, err
	}
	if !info.Mode().IsRegular() {
		return protocol.Result{}, errors.New("image must be a regular file")
	}
	if info.Size() > 2<<20 {
		return protocol.Result{}, errors.New("image exceeds 2 MiB; resize it before requesting MCP image content")
	}
	b, err := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if err != nil {
		return protocol.Result{}, err
	}
	if len(b) > 2<<20 {
		return protocol.Result{}, errors.New("image exceeds 2 MiB")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(b))
	if err != nil {
		return protocol.Result{}, fmt.Errorf("invalid or unsupported image: %w", err)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 40000000 {
		return protocol.Result{}, errors.New("image dimensions exceed 40 million pixels")
	}
	mime := map[string]string{"png": "image/png", "jpeg": "image/jpeg", "gif": "image/gif"}[format]
	if mime == "" {
		return protocol.Result{}, errors.New("only PNG, JPEG and GIF images are supported")
	}
	out := protocol.JSONResult(map[string]any{"path": path, "width": config.Width, "height": config.Height, "bytes": len(b), "mime_type": mime})
	out.Image = &protocol.Image{MIMEType: mime, Data: b}
	return out, nil
}
