package connector

import (
	"bufio"
	"context"
	"encoding/csv"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/eidonix-ai/autobot/internal/protocol"
)

// readGPUs asks nvidia-smi for each GPU's readings. A machine without GPUs reports none.
func readGPUs(ctx context.Context) ([]protocol.GPU, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=index,uuid,name,temperature.gpu,power.draw,power.limit,memory.used,memory.total,utilization.gpu",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, err
	}
	rows, err := csv.NewReader(strings.NewReader(string(out))).ReadAll()
	if err != nil {
		return nil, err
	}
	var gpus []protocol.GPU
	for _, r := range rows {
		if len(r) != 9 {
			continue
		}
		for i := range r {
			r[i] = strings.TrimSpace(r[i])
		}
		idx, _ := strconv.Atoi(r[0])
		gpus = append(gpus, protocol.GPU{Index: idx, UUID: r[1], Name: r[2], TempC: num(r[3]), PowerW: num(r[4]),
			PowerLimitW: num(r[5]), MemUsedMiB: num(r[6]), MemTotalMiB: num(r[7]), UtilPct: num(r[8])})
	}
	return gpus, nil
}

// num reads one nvidia-smi value; "[N/A]" and the like become nil.
func num(s string) *float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil
	}
	return &v
}

// readMemory returns the host's total and available memory in MiB.
func readMemory() (total, available int) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		kib, _ := strconv.Atoi(fields[1])
		switch fields[0] {
		case "MemTotal:":
			total = kib / 1024
		case "MemAvailable:":
			available = kib / 1024
		}
	}
	return total, available
}

// scanModels lists the models in dir: Hugging Face cache entries (models--org--name), folders with
// a config.json, and .gguf files, looking at most four levels down.
func scanModels(dir string) []protocol.Model {
	var models []protocol.Model
	if dir == "" {
		return nil
	}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		if strings.Count(rel, string(filepath.Separator)) >= 4 {
			return fs.SkipDir
		}
		name := d.Name()
		switch {
		case d.IsDir() && strings.HasPrefix(name, "models--"):
			org, model, ok := strings.Cut(strings.TrimPrefix(name, "models--"), "--")
			if ok {
				models = append(models, protocol.Model{Name: org + "/" + model, Path: "/models/" + rel, Bytes: size(filepath.Join(p, "blobs"))})
			}
			return fs.SkipDir
		case d.IsDir() && exists(filepath.Join(p, "config.json")):
			models = append(models, protocol.Model{Name: rel, Path: "/models/" + rel, Bytes: size(p)})
			return fs.SkipDir
		case !d.IsDir() && strings.HasSuffix(name, ".gguf"):
			info, _ := d.Info()
			if info != nil {
				models = append(models, protocol.Model{Name: rel, Path: "/models/" + rel, Bytes: info.Size()})
			}
		case d.IsDir() && (name == "blobs" || strings.HasPrefix(name, ".")):
			return fs.SkipDir
		}
		return nil
	})
	return models
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// size is the total size of the regular files under p.
func size(p string) int64 {
	var n int64
	filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}
