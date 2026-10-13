package reader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func runLayoutFixtureProcess() {
	var output string
	from, to := 0, 0
	for i := 1; i+1 < len(os.Args); i++ {
		switch os.Args[i] {
		case "--output":
			output = os.Args[i+1]
		case "--from-page":
			from, _ = strconv.Atoi(os.Args[i+1])
		case "--to-page":
			to, _ = strconv.Atoi(os.Args[i+1])
		}
	}
	if output == "" || from < 1 || to < from || to > 80 {
		os.Exit(2)
	}
	log, err := os.OpenFile(os.Getenv("FAKE_LAYOUT_LOG"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(3)
	}
	fmt.Fprintf(log, "%d-%d\n", from, to)
	log.Close()
	if err := os.MkdirAll(filepath.Join(output, "assets"), 0700); err != nil {
		os.Exit(4)
	}
	blocks := []any{}
	pages := []int{}
	encoder := json.NewEncoder(os.Stdout)
	for page := from; page <= to; page++ {
		pages = append(pages, page)
		blocks = append(blocks, map[string]any{"id": fmt.Sprintf("p%d-b1", page), "page": page, "label": "text", "bounds": map[string]float64{"x": 0.1, "y": 0.1, "width": 0.5, "height": 0.3}, "text": strings.Repeat("Sentence number. ", 60)})
		encoder.Encode(map[string]any{"event": "page", "page": page, "total": 80})
	}
	manifest, err := json.Marshal(map[string]any{"pages": 80, "model": "fake", "parsedPages": pages, "blocks": blocks, "warnings": []any{}, "incompletePages": []int{}})
	if err != nil {
		os.Exit(5)
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), manifest, 0600); err != nil {
		os.Exit(6)
	}
}
