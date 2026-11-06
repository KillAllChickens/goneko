package main

import (
	"embed"
	"fmt"
	"image/png"
	"math"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/vcaesar/go-wayland/client"
	wlr_layer_shell "github.com/vcaesar/go-wayland/wlr/layer-shell-v1"
	"golang.org/x/sys/unix"
)

//go:embed oneko_frames/*.png
var framesFS embed.FS

const (
	Size           = 32
	BufferSizeByte = Size * Size * 4
)

type CatState int

const (
	StateChasing CatState = iota
	StateSitting
	StateWashing
	StateSleeping
)

type Dir int

const (
	DirN Dir = iota
	DirNE
	DirE
	DirSE
	DirS
	DirSW
	DirW
	DirNW
)

type PetAction int

const (
	ActionWandering PetAction = iota
	ActionSitting
	ActionWashing
	ActionSleeping
)

func dirFromDelta(dx, dy float32) Dir {
	adx := float32(math.Abs(float64(dx)))
	ady := float32(math.Abs(float64(dy)))

	if adx > ady*2.0 {
		if dx > 0 {
			return DirE
		}
		return DirW
	} else if ady > adx*2.0 {
		if dy > 0 {
			return DirS
		}
		return DirN
	}

	if dx >= 0 && dy <= 0 {
		return DirNE
	} else if dx < 0 && dy <= 0 {
		return DirNW
	} else if dx >= 0 && dy > 0 {
		return DirSE
	}
	return DirSW
}

type Frame struct {
	pixels []byte // 32x32x4 in Wayland ARGB8888 (Little Endian: [B, G, R, A])
	solid  [Size][Size]bool
}

type OutputSurface struct {
	outputID   uint32
	output     *client.Output
	surface    *client.Surface
	layerSurf  *wlr_layer_shell.LayerSurface
	configured bool

	x, y, w, h     float32
	localX, localY float32

	shmBuf  *client.Buffer
	shmData []byte

	lastDrawnX int32
	lastDrawnY int32
	lastFrame  string
	lastActive *bool
}

type Cat struct {
	display    *client.Display
	ctx        *client.Context
	compositor *client.Compositor
	shm        *client.Shm
	layerShell *wlr_layer_shell.LayerShell
	seat       *client.Seat
	pointer    *client.Pointer

	outputs        []*OutputSurface
	activeOutputID *uint32

	winX, winY float32
	dir        Dir
	frame      bool

	action      PetAction
	actionTicks uint32
	mouseDown   bool
	targetPos   *[2]float32

	frames map[string]*Frame
	exit   bool
}

func loadFrames() (map[string]*Frame, error) {
	frames := make(map[string]*Frame)
	entries, err := framesFS.ReadDir("oneko_frames")
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".png") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".png")

		file, err := framesFS.Open("oneko_frames/" + entry.Name())
		if err != nil {
			return nil, err
		}
		img, err := png.Decode(file)
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("decoding %s: %w", entry.Name(), err)
		}

		f := &Frame{
			pixels: make([]byte, BufferSizeByte),
		}

		for y := 0; y < Size; y++ {
			for x := 0; x < Size; x++ {
				r, g, b, a := img.At(x, y).RGBA()
				a8 := byte(a >> 8)
				r8 := byte(r >> 8)
				g8 := byte(g >> 8)
				b8 := byte(b >> 8)

				offset := (y*Size + x) * 4
				f.pixels[offset+0] = b8
				f.pixels[offset+1] = g8
				f.pixels[offset+2] = r8
				f.pixels[offset+3] = a8

				if a8 > 0 {
					f.solid[y][x] = true
				}
			}
		}
		frames[name] = f
	}

	return frames, nil
}

func (c *Cat) createShmBuffer() (*client.Buffer, []byte, error) {
	fd, err := unix.MemfdCreate("oneko-shm", unix.MFD_CLOEXEC)
	if err != nil {
		return nil, nil, err
	}
	defer unix.Close(fd)

	if err := unix.Ftruncate(fd, int64(BufferSizeByte)); err != nil {
		return nil, nil, err
	}

	data, err := unix.Mmap(fd, 0, BufferSizeByte, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return nil, nil, err
	}

	pool, err := c.shm.CreatePool(fd, BufferSizeByte)
	if err != nil {
		_ = unix.Munmap(data)
		return nil, nil, err
	}
	defer pool.Destroy()

	buf, err := pool.CreateBuffer(0, Size, Size, Size*4, uint32(client.ShmFormatArgb8888))
	if err != nil {
		_ = unix.Munmap(data)
		return nil, nil, err
	}

	return buf, data, nil
}

func (c *Cat) setupOutputSurface(out *OutputSurface) error {
	surf, err := c.compositor.CreateSurface()
	if err != nil {
		return err
	}
	out.surface = surf

	layerSurf, err := c.layerShell.GetLayerSurface(
		surf,
		out.output,
		uint32(wlr_layer_shell.LayerShellLayerOverlay),
		"oneko",
	)
	if err != nil {
		return err
	}
	out.layerSurf = layerSurf

	layerSurf.SetAnchor(uint32(wlr_layer_shell.LayerSurfaceAnchorTop | wlr_layer_shell.LayerSurfaceAnchorLeft))
	layerSurf.SetSize(Size, Size)
	layerSurf.SetExclusiveZone(-1)
	layerSurf.SetKeyboardInteractivity(uint32(wlr_layer_shell.LayerSurfaceKeyboardInteractivityNone))

	layerSurf.SetConfigureHandler(func(e wlr_layer_shell.LayerSurfaceConfigureEvent) {
		layerSurf.AckConfigure(e.Serial)
		out.configured = true
	})

	buf, data, err := c.createShmBuffer()
	if err != nil {
		return err
	}
	out.shmBuf = buf
	out.shmData = data

	return surf.Commit()
}

func (c *Cat) pickOutputIDForPoint(x, y float32) *uint32 {
	for _, entry := range c.outputs {
		if x >= entry.x && x < entry.x+entry.w && y >= entry.y && y < entry.y+entry.h {
			id := entry.outputID
			return &id
		}
	}
	if len(c.outputs) > 0 {
		id := c.outputs[0].outputID
		return &id
	}
	return nil
}

func (c *Cat) draw(out *OutputSurface, frame *Frame, isActive bool) {
	if isActive && frame != nil {
		copy(out.shmData, frame.pixels)
	} else {
		for i := range out.shmData {
			out.shmData[i] = 0
		}
	}

	region, err := c.compositor.CreateRegion()
	if err == nil {
		for y := 0; y < Size; y++ {
			var startX *int
			for x := 0; x < Size; x++ {
				isInputPixel := true
				if isActive && frame != nil {
					isInputPixel = frame.solid[y][x]
				}

				if startX == nil && isInputPixel {
					sx := x
					startX = &sx
				} else if startX != nil && !isInputPixel {
					region.Add(int32(*startX), int32(y), int32(x-*startX), 1)
					startX = nil
				}
			}
			if startX != nil {
				region.Add(int32(*startX), int32(y), int32(Size-*startX), 1)
			}
		}
		out.surface.SetInputRegion(region)
		region.Destroy()
	}

	_ = out.surface.Attach(out.shmBuf, 0, 0)
	_ = out.surface.Damage(0, 0, Size, Size)
	_ = out.surface.Commit()
}

func (c *Cat) tick() {
	if len(c.outputs) == 0 {
		return
	}

	if c.activeOutputID == nil && len(c.outputs) > 0 {
		id := c.outputs[0].outputID
		c.activeOutputID = &id
	}

	var curOutput *OutputSurface
	for _, o := range c.outputs {
		if c.activeOutputID != nil && o.outputID == *c.activeOutputID {
			curOutput = o
			break
		}
	}
	if curOutput == nil {
		curOutput = c.outputs[0]
	}

	curMinX := curOutput.x
	curMinY := curOutput.y
	curMaxX := float32(math.Max(float64(curOutput.x+curOutput.w-Size), float64(curMinX)))
	curMaxY := float32(math.Max(float64(curOutput.y+curOutput.h-Size), float64(curMinY)))

	c.frame = !c.frame
	chasingPointer := false

	if c.targetPos != nil {
		tx := c.targetPos[0]
		ty := c.targetPos[1]

		dx := tx - (c.winX + Size*0.5)
		dy := ty - (c.winY + Size*0.5)
		dist := float32(math.Hypot(float64(dx), float64(dy)))

		if dist < 4.0 && !c.mouseDown {
			c.targetPos = nil
			c.action = ActionSitting
			c.actionTicks = 8 * 20
		} else {
			chasingPointer = true
			if math.Abs(float64(dx)) > 2.0 || math.Abs(float64(dy)) > 2.0 {
				c.dir = dirFromDelta(dx, dy)
			}

			speed := float32(12.0)
			if dist > speed {
				c.winX += (dx / dist) * speed
				c.winY += (dy / dist) * speed
			} else {
				c.winX += dx
				c.winY += dy
			}
		}
	}

	var state CatState
	if chasingPointer {
		state = StateChasing
	} else {
		if c.actionTicks == 0 {
			r := rand.Intn(100)
			dirs := []Dir{DirN, DirNE, DirE, DirSE, DirS, DirSW, DirW, DirNW}

			switch c.action {
			case ActionWandering:
				if r < 30 {
					c.actionTicks = 20
					c.action = ActionSitting
				} else {
					c.actionTicks = uint32(15 + rand.Intn(15))
					c.action = ActionWandering
					c.dir = dirs[rand.Intn(8)]
				}
			case ActionSitting:
				if r < 30 {
					c.actionTicks = 20
					c.action = ActionWashing
				} else if r < 60 {
					c.actionTicks = uint32(15 + rand.Intn(15))
					c.action = ActionWandering
					c.dir = dirs[rand.Intn(8)]
				} else {
					c.actionTicks = 20
					c.action = ActionSitting
				}
			case ActionWashing:
				if r < 50 {
					c.actionTicks = 40
					c.action = ActionSleeping
				} else if r < 70 {
					c.actionTicks = 20
					c.action = ActionSitting
				} else {
					c.actionTicks = uint32(15 + rand.Intn(15))
					c.action = ActionWandering
					c.dir = dirs[rand.Intn(8)]
				}
			case ActionSleeping:
				if r < 20 {
					c.actionTicks = 20
					c.action = ActionSitting
				} else {
					c.actionTicks = 40
					c.action = ActionSleeping
				}
			}
		} else {
			c.actionTicks--
		}

		switch c.action {
		case ActionSitting:
			state = StateSitting
		case ActionWashing:
			state = StateWashing
		case ActionSleeping:
			state = StateSleeping
		case ActionWandering:
			speed := float32(8.0)
			var dx, dy float32
			switch c.dir {
			case DirN:
				dy = -speed
			case DirS:
				dy = speed
			case DirE:
				dx = speed
			case DirW:
				dx = -speed
			case DirNE:
				dx, dy = speed, -speed
			case DirNW:
				dx, dy = -speed, -speed
			case DirSE:
				dx, dy = speed, speed
			case DirSW:
				dx, dy = -speed, speed
			}

			c.winX += dx
			c.winY += dy

			if c.winX <= curMinX || c.winX >= curMaxX || c.winY <= curMinY || c.winY >= curMaxY {
				c.actionTicks = 0
			}
			state = StateChasing
		}
	}

	if !chasingPointer {
		c.winX = float32(math.Min(math.Max(float64(c.winX), float64(curMinX)), float64(curMaxX)))
		c.winY = float32(math.Min(math.Max(float64(c.winY), float64(curMinY)), float64(curMaxY)))
	}

	var frameName string
	switch state {
	case StateSitting:
		frameName = "cat_sitting"
	case StateWashing:
		if c.frame {
			frameName = "cat_washing1"
		} else {
			frameName = "cat_washing2"
		}
	case StateSleeping:
		if (c.actionTicks/4)%2 == 0 {
			frameName = "cat_sleeping1"
		} else {
			frameName = "cat_sleeping2"
		}
	case StateChasing:
		switch c.dir {
		case DirE:
			if c.frame {
				frameName = "cat_right1"
			} else {
				frameName = "cat_right2"
			}
		case DirW:
			if c.frame {
				frameName = "cat_left1"
			} else {
				frameName = "cat_left2"
			}
		case DirN:
			if c.frame {
				frameName = "cat_up1"
			} else {
				frameName = "cat_up2"
			}
		case DirS:
			if c.frame {
				frameName = "cat_down1"
			} else {
				frameName = "cat_down2"
			}
		case DirNE:
			if c.frame {
				frameName = "cat_ne1"
			} else {
				frameName = "cat_ne2"
			}
		case DirNW:
			if c.frame {
				frameName = "cat_nw1"
			} else {
				frameName = "cat_nw2"
			}
		case DirSE:
			if c.frame {
				frameName = "cat_se1"
			} else {
				frameName = "cat_se2"
			}
		case DirSW:
			if c.frame {
				frameName = "cat_sw1"
			} else {
				frameName = "cat_sw2"
			}
		}
	}

	c.activeOutputID = c.pickOutputIDForPoint(c.winX, c.winY)

	for _, out := range c.outputs {
		if !out.configured {
			continue
		}

		isActive := c.activeOutputID != nil && out.outputID == *c.activeOutputID
		localMaxX := float32(math.Max(float64(out.w-Size), 0))
		localMaxY := float32(math.Max(float64(out.h-Size), 0))

		out.localX = float32(math.Min(math.Max(float64(c.winX-out.x), 0), float64(localMaxX)))
		out.localY = float32(math.Min(math.Max(float64(c.winY-out.y), 0), float64(localMaxY)))

		xInt := int32(out.localX)
		yInt := int32(out.localY)

		changed := out.lastDrawnX != xInt ||
			out.lastDrawnY != yInt ||
			out.lastFrame != frameName ||
			out.lastActive == nil ||
			*out.lastActive != isActive

		if changed {
			out.layerSurf.SetMargin(yInt, 0, 0, xInt)
			var targetFrame *Frame
			if isActive {
				targetFrame = c.frames[frameName]
			}
			c.draw(out, targetFrame, isActive)

			out.lastDrawnX = xInt
			out.lastDrawnY = yInt
			out.lastFrame = frameName
			out.lastActive = &isActive
		}
	}
}

func (c *Cat) roundtrip() error {
	cb, err := c.display.Sync()
	if err != nil {
		return err
	}
	done := false
	cb.SetDoneHandler(func(e client.CallbackDoneEvent) {
		done = true
		_ = cb.Destroy()
	})

	for !done {
		if err := c.ctx.Dispatch(); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cat) setupPointer() {
	if c.pointer == nil {
		return
	}

	findOutputForSurface := func(surfID uint32) *OutputSurface {
		for _, o := range c.outputs {
			if o.surface != nil && o.surface.ID() == surfID {
				return o
			}
		}
		return nil
	}

	c.pointer.SetEnterHandler(func(e client.PointerEnterEvent) {
		if o := findOutputForSurface(e.Surface.ID()); o != nil {
			c.targetPos = &[2]float32{
				o.x + o.localX + float32(e.SurfaceX),
				o.y + o.localY + float32(e.SurfaceY),
			}
		}
	})

	c.pointer.SetMotionHandler(func(e client.PointerMotionEvent) {
		if c.activeOutputID != nil {
			for _, o := range c.outputs {
				if o.outputID == *c.activeOutputID {
					c.targetPos = &[2]float32{
						o.x + o.localX + float32(e.SurfaceX),
						o.y + o.localY + float32(e.SurfaceY),
					}
					break
				}
			}
		}
	})

	c.pointer.SetButtonHandler(func(e client.PointerButtonEvent) {
		if e.State == uint32(client.PointerButtonStatePressed) {
			c.mouseDown = true
			c.action = ActionSitting
			c.actionTicks = 0
		} else {
			c.mouseDown = false
		}
	})
}

func main() {
	frames, err := loadFrames()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading frames: %v\n", err)
		os.Exit(1)
	}

	display, err := client.Connect("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to Wayland: %v\n", err)
		os.Exit(1)
	}
	ctx := display.Context()

	cat := &Cat{
		display:     display,
		ctx:         ctx,
		frames:      frames,
		winX:        200,
		winY:        200,
		dir:         DirE,
		action:      ActionSitting,
		actionTicks: 0,
	}

	reg, err := display.GetRegistry()
	if err != nil {
		panic(err)
	}

	type pendingOutput struct {
		id     uint32
		output *client.Output
		x, y   int32
		w, h   int32
	}
	var pending []*pendingOutput

	reg.SetGlobalHandler(func(e client.RegistryGlobalEvent) {
		switch e.Interface {
		case "wl_compositor":
			cat.compositor = client.NewCompositor(ctx)
			_ = reg.Bind(e.Name, e.Interface, 4, cat.compositor)
		case "wl_shm":
			cat.shm = client.NewShm(ctx)
			_ = reg.Bind(e.Name, e.Interface, 1, cat.shm)
		case "zwlr_layer_shell_v1":
			cat.layerShell = wlr_layer_shell.NewLayerShell(ctx)
			_ = reg.Bind(e.Name, e.Interface, 4, cat.layerShell)
		case "wl_seat":
			cat.seat = client.NewSeat(ctx)
			_ = reg.Bind(e.Name, e.Interface, 5, cat.seat)
			cat.seat.SetCapabilitiesHandler(func(se client.SeatCapabilitiesEvent) {
				if se.Capabilities&uint32(client.SeatCapabilityPointer) != 0 && cat.pointer == nil {
					ptr, err := cat.seat.GetPointer()
					if err == nil {
						cat.pointer = ptr
						cat.setupPointer()
					}
				}
			})
		case "wl_output":
			out := client.NewOutput(ctx)
			_ = reg.Bind(e.Name, e.Interface, 4, out)
			po := &pendingOutput{id: e.Name, output: out}
			pending = append(pending, po)

			out.SetGeometryHandler(func(ge client.OutputGeometryEvent) {
				po.x = ge.X
				po.y = ge.Y
			})
			out.SetModeHandler(func(me client.OutputModeEvent) {
				if me.Flags&uint32(client.OutputModeCurrent) != 0 {
					po.w = me.Width
					po.h = me.Height
				}
			})
		}
	})

	if err := cat.roundtrip(); err != nil {
		panic(err)
	}

	if err := cat.roundtrip(); err != nil {
		panic(err)
	}

	if cat.compositor == nil || cat.shm == nil || cat.layerShell == nil {
		fmt.Fprintln(os.Stderr, "Error: Compositor must support wl_compositor, wl_shm, and zwlr_layer_shell_v1")
		os.Exit(1)
	}

	for _, po := range pending {
		outSurf := &OutputSurface{
			outputID: po.id,
			output:   po.output,
			x:        float32(po.x),
			y:        float32(po.y),
			w:        float32(po.w),
			h:        float32(po.h),
		}
		if err := cat.setupOutputSurface(outSurf); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to setup surface for output %d: %v\n", po.id, err)
			continue
		}
		cat.outputs = append(cat.outputs, outSurf)
	}

	// Main tick loop
	for !cat.exit {
		hasConfigured := false
		for _, o := range cat.outputs {
			if o.configured {
				hasConfigured = true
				break
			}
		}

		if hasConfigured {
			cat.tick()
		}

		if err := cat.roundtrip(); err != nil {
			break
		}
		time.Sleep(125 * time.Millisecond)
	}
}
