// Package main provides a terminal-based video player application that renders
// video streams as real-time colored ASCII art utilizing mpv for media decoding
// and Bubble Tea for the terminal interface.
package main

import (
	"bytes"
	"fmt"
	"log"
	"log/slog"
	"math"
	"math/bits"
	"os"
	"os/exec"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/term"
	"github.com/gen2brain/go-mpv"
	"github.com/spf13/pflag"
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
	kernelWidth   = 5
	kernelHeight  = kernelWidth * 2
	edgeThreshold = 45
)

var (
	debugFile    string
	imageTxtFile string
	brightness   float64 = 0.1
	imageWidth   int     = 80
	imageHeight  int     = 40
	skipFilter   bool
)

func init() {
	w, h, err := term.GetSize(os.Stdout.Fd())
	if err == nil {
		imageWidth = w
		imageHeight = h
	}
	pflag.StringVarP(&debugFile, "debug", "d", "", "path to output log file")
	pflag.StringVarP(&imageTxtFile, "image", "i", "", "render image instead of video")
	pflag.Float64VarP(&brightness, "brightness", "b", brightness, "frame brightness of video")
	pflag.IntVarP(&imageWidth, "width", "w", imageWidth, "image mode output width in characters")
	pflag.IntVarP(&imageHeight, "height", "h", imageHeight, "image mode output height in characters")
	pflag.BoolVarP(&skipFilter, "skip-filter", "s", skipFilter, "skip prefilter for image and video")
}

// initialModel configures libmpv with software rendering and edge-detection filters.
func initialModel(videoPath string) (*model, error) {
	m := mpv.New()
	if err := m.SetOptionString("vo", "libmpv"); err != nil {
		return nil, fmt.Errorf("failed to set vo option: %w", err)
	}

	if !skipFilter {
		// Filter pipeline extracts structural edges and blends them back into the main video stream
		// to improve line recognition during ASCII bitmask analysis.
		filter := fmt.Sprintf(`
		[vid1]split[main][orig];
		[orig]edgedetect=low=0.2:high=0.4,dilation=threshold0=255,negate,eq=gamma=0.5[edge];
		[main][edge]blend=c0_mode=multiply,eq=brightness=%.2f[vo]
		`, brightness)
		if err := m.SetPropertyString("lavfi-complex", filter); err != nil {
			return nil, fmt.Errorf("failed to set vo option: %w", err)
		}
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
	const totalPixel = uint64(kernelWidth * kernelHeight)
	var highLum, lowLum, totalLums float64
	var lums [totalPixel]byte

	var rt, gt, bt uint64
	var i int
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
			totalLums += lum
			highLum = max(lum, highLum)
			lowLum = min(lum, lowLum)
			lums[i] = byte(lum)
			i++
		}
	}

	var bitmap uint64
	if highLum-lowLum > edgeThreshold {
		avg := byte(totalLums / float64(totalPixel))
		for i, l := range lums {
			if l > avg {
				bitmap |= (1 << i)
			}
		}
	} else {
		for i, l := range lums {
			if l > 127 {
				bitmap |= (1 << i)
			}
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
	if char == '@' || char == ' ' {
		const lumset = " .:;+*?%#@"
		const l = float64(len(lumset))
		lum := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
		idx := min(math.Floor((lum/260)*l), l-1)
		char = lumset[int(idx)]
		return
	}

	return
}

func buildFrame(buf []byte, w, h, stride int) string {
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
	return frameBuff.String()
}

func renderImage(path string, w, h int) (string, error) {
	renderW := w * kernelWidth
	renderH := h * kernelHeight
	stride := renderW * 4
	var filter string

	if skipFilter {
		filter = fmt.Sprintf(
			`[0:v]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,format=rgba[vo]`,
			renderW, renderH, renderW, renderH,
		)
	} else {
		filter = fmt.Sprintf(
			`[0:v]scale=%d:%d:force_original_aspect_ratio=decrease,pad=%d:%d:(ow-iw)/2:(oh-ih)/2,format=rgba,split[main][orig];`+
				`[orig]edgedetect=low=0.2:high=0.4,dilation=threshold0=255,negate,eq=gamma=0.5[edge];`+
				`[main][edge]blend=c0_mode=multiply,eq=brightness=%.2f,format=rgba[vo]`,
			renderW, renderH, renderW, renderH, brightness,
		)
	}

	cmd := exec.Command("ffmpeg",
		"-v", "error",
		"-i", path,
		"-filter_complex", filter,
		"-map", "[vo]",
		"-frames:v", "1",
		"-f", "rawvideo",
		"-pix_fmt", "rgba",
		"-",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ffmpeg failed: %w: %s", err, stderr.String())
	}
	if len(out) < stride*renderH {
		return "", fmt.Errorf("ffmpeg returned %d bytes, expected %d", len(out), stride*renderH)
	}

	return buildFrame(out, w, h, stride), nil
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
				return nil
			}
		}

		m.mu.Lock()
		if m.width == 0 || m.height == 0 {
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

		return frameMsg(buildFrame(buf, w, h, stride))
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
	pflag.Parse()

	args := pflag.Args()
	if len(args) < 1 {
		fmt.Println("Usage: go run main.go [-debug <logpath>] [-image <image.txt>] <videopath>")
		os.Exit(1)
	}

	if debugFile != "" {
		f, err := os.OpenFile(debugFile, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
		if err != nil {
			log.Fatalf("failed to open debug log file: %v", err)
		}
		defer f.Close()
		f.WriteString("started\n")
		slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
	} else {
		slog.SetDefault(slog.New(slog.DiscardHandler))
	}

	if imageTxtFile != "" {
		frame, err := renderImage(args[0], imageWidth, imageHeight)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			os.Exit(1)
		}

		os.Stdout.WriteString(frame)
		err = os.WriteFile(imageTxtFile, []byte(frame), 0o644)
		if err != nil {
			fmt.Printf("Error writing image: %v\n", err)
			os.Exit(1)
		}
		return
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
