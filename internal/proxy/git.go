package proxy

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

func (p *Proxy) authorizeGit(w http.ResponseWriter, r *http.Request, path string) error {
	if r.Method == "GET" || r.Method == "HEAD" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 6 && strings.Join(parts[2:], "/") == "info/lfs/objects/batch" && r.Method == "POST" && r.URL.RawQuery == "" {
		body, err := readJSONBody(r)
		if err == nil && body["operation"] == "download" {
			return nil
		}
		return errDenied
	}
	if len(parts) != 3 || r.Method != "POST" || r.URL.RawQuery != "" {
		return errDenied
	}
	if parts[2] == "git-upload-pack" {
		return nil
	} // Git's read-only fetch RPC.
	if parts[2] != "git-receive-pack" || r.Header.Get("Content-Type") != "application/x-git-receive-pack-request" || !p.repoAllowed(r, parts[0], strings.TrimSuffix(parts[1], ".git")) {
		return errDenied
	}
	r.Body = http.MaxBytesReader(w, r.Body, p.config.MaxPushBytes)
	header, err := p.readCommands(r.Body)
	if err != nil {
		return err
	}
	r.Body = &replayBody{Reader: io.MultiReader(bytes.NewReader(header), r.Body), Closer: r.Body}
	return nil
}

type replayBody struct {
	io.Reader
	io.Closer
}

func (p *Proxy) readCommands(r io.Reader) ([]byte, error) {
	var header bytes.Buffer
	commands := 0
	seen := map[string]bool{}
	for {
		var size [4]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return nil, fmt.Errorf("truncated Git command")
		}
		header.Write(size[:])
		n, err := strconv.ParseUint(string(size[:]), 16, 16)
		if err != nil {
			return nil, fmt.Errorf("invalid Git packet length")
		}
		if n == 0 {
			if commands == 0 {
				return nil, fmt.Errorf("empty Git push")
			}
			return header.Bytes(), nil
		}
		if n < 4 || n > 65520 || header.Len()+int(n) > 1<<20 {
			return nil, fmt.Errorf("invalid or oversized Git commands")
		}
		b := make([]byte, int(n)-4)
		if _, err = io.ReadFull(r, b); err != nil {
			return nil, fmt.Errorf("truncated Git packet")
		}
		header.Write(b)
		line := strings.TrimSuffix(string(b), "\n")
		if command, caps, ok := strings.Cut(line, "\x00"); ok {
			if commands != 0 {
				return nil, errDenied
			}
			line = command
			for _, cap := range strings.Split(caps, " ") {
				switch cap {
				case "", "report-status", "report-status-v2", "side-band-64k", "quiet", "atomic", "ofs-delta", "object-format=sha1":
				default:
					if !strings.HasPrefix(cap, "agent=") || strings.ContainsAny(cap, "\x00\r\n") {
						return nil, fmt.Errorf("unsupported Git capability %q", cap)
					}
				}
			}
		}
		fields := strings.Split(line, " ")
		if len(fields) != 3 || !objectID(fields[0]) || !objectID(fields[1]) || !strings.HasPrefix(fields[2], "refs/heads/") || !p.branch(strings.TrimPrefix(fields[2], "refs/heads/")) || seen[fields[2]] {
			return nil, errDenied
		}
		seen[fields[2]] = true
		commands++
	}
}

func objectID(s string) bool {
	if len(s) != 40 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
