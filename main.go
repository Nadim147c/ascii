// Package main provides a terminal-based video player application that renders
// video streams as real-time colored ASCII art utilizing mpv for media decoding
// and Bubble Tea for the terminal interface.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"math"
	"math/bits"
	"os"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/gen2brain/go-mpv"
)

//go:generate go run ./bitmaps

type frameMsg string

type model struct {
	mu        sync.Mutex
	height    int
	width     int
	stride    int
	mpvClient *mpv.Mpv
	renderCtx *mpv.RenderContext
	buf       []byte
	frame     string
	done      bool
}

const (
	// Aspect ratio dimensions for pixel patch extraction.
	kernelWidth  = 7
	kernelHeight = kernelWidth * 2
)

// initialModel configures libmpv with software rendering and edge-detection filters.
func initialModel(videoPath string) (*model, error) {
	m := mpv.New()
	if err := m.SetOptionString("vo", "libmpv"); err != nil {
		return nil, fmt.Errorf("failed to set vo option: %w", err)
	}

	if err := m.SetOptionString("keep-open", "yes"); err != nil {
		return nil, fmt.Errorf("failed to set keep-open option: %w", err)
	}

	// Filter pipeline extracts structural edges and blends them back into the main video stream
	// to improve line recognition during ASCII bitmask analysis.
	filter := `
	[vid1]split[main][orig];
	[orig]edgedetect=low=0.2:high=0.4,dilation=threshold0=255,negate,eq=gamma=0.5[edge];
	[main][edge]blend=c0_mode=multiply,eq=brightness=0.1[vo]
	`
	if err := m.SetPropertyString("lavfi-complex", filter); err != nil {
		return nil, fmt.Errorf("failed to set vo option: %w", err)
	}

	if err := m.Initialize(); err != nil {
		return nil, fmt.Errorf("failed to initialize mpv: %w", err)
	}

	rc, err := m.NewRenderContextSW()
	if err != nil {
		return nil, fmt.Errorf("failed to create render context: %w", err)
	}

	initialWidth := 80
	initialHeight := 40
	renderW := initialWidth * kernelWidth
	renderH := initialHeight * kernelHeight
	stride := renderW * 4
	buf := make([]byte, stride*renderH)

	if err := m.Command([]string{"loadfile", videoPath}); err != nil {
		return nil, fmt.Errorf("failed to load video: %w", err)
	}

	return &model{
		width:     initialWidth,
		height:    initialHeight,
		stride:    stride,
		mpvClient: m,
		renderCtx: rc,
		buf:       buf,
	}, nil
}

func (m *model) Init() tea.Cmd {
	return m.renderNextFrame()
}

// analyzePatch converts a kernel-sized RGB pixel block into a bitmask, evaluates Hamming
// distance against precalculated glyph masks, and computes average ANSI color values.
func analyzePatch(buf []byte, startX, startY, stride int) (char, r, g, b byte) {
	var i int
	var bitmap uint64

	totalPixel := uint64(kernelWidth * kernelHeight)
	var rt, gt, bt uint64
	for dy := range kernelHeight {
		for dx := range kernelWidth {
			px := startX + dx
			py := startY + dy
			offset := (py * stride) + (px * 4)
			r, g, b := buf[offset], buf[offset+1], buf[offset+2]

			rt += uint64(r)
			gt += uint64(g)
			bt += uint64(b)

			lum := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
			if lum > 170.0 {
				bitmap |= (1 << i)
			}
			i++
		}
	}

	diff := kernelWidth * kernelHeight
	char = ' '

	for i := range bitmaps {
		newDiff := bits.OnesCount64(bitmaps[i] ^ bitmap)
		if newDiff < diff {
			diff = newDiff
			char = chars[i]
		}
	}

	r, g, b = byte(rt/totalPixel), byte(gt/totalPixel), byte(bt/totalPixel)

	// Dynamic fallback mapping when glyph masks map to dense or empty regions.
	if char == '@' {
		const lumset = ":;+*?%#@"
		const l = float64(len(lumset))
		lum := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
		idx := min(math.Floor((lum/260)*l), l-1)
		char = lumset[int(idx)]
		return
	}

	if char == ' ' {
		const lumset = ".:;+*"
		const l = float64(len(lumset))
		lum := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
		if lum > 20 {
			idx := min(math.Floor((lum/260)*l), l-1)
			char = lumset[int(idx)]
		}
	}

	return
}

// renderNextFrame ticks frame decoding using mpv's software renderer and constructs formatted ANSI output.
func (m *model) renderNextFrame() tea.Cmd {
	return func() tea.Msg {
		time.Sleep(33 * time.Millisecond)

		for {
			event := m.mpvClient.WaitEvent(0)
			if event.Error != nil {
				return nil
			}
			if event.EventID == mpv.EventNone {
				break
			}
			if event.EventID == mpv.EventLogMsg {
				l := event.LogMessage()
				slog.Info("mpv", "prefix", l.Prefix, "level", l.Level, "msg", l.Text)
			}
			if event.EventID == mpv.EventEnd {
				return tea.QuitMsg{}
			}
		}

		m.mu.Lock()
		if m.done || m.width == 0 || m.height == 0 {
			m.mu.Unlock()
			return frameMsg("")
		}

		w, h := m.width, m.height
		stride := m.stride
		buf := m.buf
		rc := m.renderCtx
		m.mu.Unlock()

		renderW := w * kernelWidth
		renderH := h * kernelHeight

		err := rc.RenderSW(renderW, renderH, stride, "rgba", buf)
		if err != nil {
			return frameMsg("")
		}

		var frameBuff bytes.Buffer

		for y := range h {
			for x := range w {
				char, r, g, b := analyzePatch(buf, x*kernelWidth, y*kernelHeight, stride)
				fmt.Fprintf(&frameBuff, "\x1b[38;2;%d;%d;%dm%c", r, g, b, char)
			}
			log.Println(y)
			frameBuff.WriteString("\n")
		}
		frameBuff.WriteString("\x1b[0m")
		return frameMsg(frameBuff.Bytes())
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// Reallocate frame buffer when terminal dimensions change.
		m.mu.Lock()
		m.width = msg.Width
		m.height = msg.Height
		renderW := m.width * kernelWidth
		m.stride = renderW * 4
		m.buf = make([]byte, m.stride*(m.height*kernelHeight))
		m.mu.Unlock()

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.mu.Lock()
			m.done = true
			m.mu.Unlock()
			return m, tea.Quit
		case "space":
			m.mpvClient.CommandString("cycle pause")
		case "right":
			m.mpvClient.CommandString("seek 5")
		case "left":
			m.mpvClient.CommandString("seek -5")
		}

	case frameMsg:
		m.frame = string(msg)
		return m, m.renderNextFrame()
	}

	return m, nil
}

func (m *model) View() tea.View {
	v := tea.NewView(m.frame)
	v.AltScreen = true
	return v
}

func main() {
	debugFile := flag.String("debug", "", "path to output log file")
	flag.Parse()

	args := flag.Args()
	if len(args) < 1 {
		fmt.Println("Usage: go run main.go [-debug <logpath>] <videopath>")
		os.Exit(1)
	}

	if *debugFile != "" {
		f, err := os.OpenFile(*debugFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			log.Fatalf("failed to open debug log file: %v", err)
		}
		defer f.Close()
		f.WriteString("started\n")
		slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
	} else {
		slog.SetDefault(slog.New(slog.DiscardHandler))
	}

	m, err := initialModel(args[0])
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		os.Exit(1)
	}

	defer func() {
		if m.renderCtx != nil {
			m.renderCtx.Free()
		}
		if m.mpvClient != nil {
			m.mpvClient.Destroy()
		}
	}()

	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		fmt.Printf("Error running app: %v\n", err)
		os.Exit(1)
	}
}
