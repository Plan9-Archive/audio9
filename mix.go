package audio9

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

func dbg(s string, argv ...interface{}) {
	fmt.Fprintf(os.Stderr, s+"\n", argv...)
}

type WaitSetting bool

const (
	aBUF   = 8 * 1024
	aDELAY = 2048
	aCHAN  = 2
	aFREQ  = 44100
)

// Sink encapsulates a place to send audio.
// Length will return the amount of buffer space available.
// Idle instructs the Sink to close open file handles until the next Write.
type Sink interface {
	io.WriteCloser

	// Ready returns whether the Sink is ready for writing.
	Ready() bool

	// Length returns the amount of used space in the Sink's buffer.
	// If no buffering is to be used, Length returns -1.
	Length() int64

	// Idle instructs the Sink to free any assocated resources until the next Write.
	Idle()
}

type DevSink struct {
	fname string
	f *os.File
}

func NewDevSink(fname string) *DevSink {
	return &DevSink{fname: fname}
}

func (d *DevSink) Write(p []byte) (int, error) {
	var err error

	if d.f == nil {
		d.f, err = os.OpenFile(d.fname, os.O_WRONLY, 0)
		if err != nil {
			return 0, err
		}
	}

	if len(p) == 0 {
		return 0, nil
	}

	return d.f.Write(p)
}

func (d *DevSink) Close() error {
	if d.f != nil {
		if err := d.f.Close(); err != nil {
			return err
		} else {
			d.f = nil
			return nil
		}
	}
	return nil
}

func (d *DevSink) Ready() bool {
	return d.f != nil
}

func (d *DevSink) Length() int64 {
	if d.f == nil {
		return -1
	}
	if len, err := d.f.Seek(0, 2); err != nil {
		dbg("%s Length: %s", d.fname, err)
		return -1
	} else {
		return len
	}
}

func (d *DevSink) Idle() {
	d.Close()
}

type WriterSink struct {
	io.WriteCloser
}

func NewWriterSink(wr io.WriteCloser) *WriterSink {
	return &WriterSink{wr}
}

func (w *WriterSink) Ready() bool {
	return true
}

func (w *WriterSink) Length() int64 {
	return -1
}

func (w *WriterSink) Idle() {
}

// Stream represents one stream in the Mixer.
// It implements io.WriteCloser.
type Stream struct {
	id    int
	mixer *Mixer
	run   bool
	used  bool
	wp    int
	mu    sync.Mutex
	cond  *sync.Cond
}

func (s Stream) String() string {
	return fmt.Sprintf("Stream %d run %t used %t wp %d", s.id, s.run, s.used, s.wp)
}

func (s *Stream) Write(p []byte) (int, error) {
	if s.used == false {
		return 0, fmt.Errorf("stream closed")
	}

	wc := len(p)
	n := wc / (aCHAN * 2)

	s.mu.Lock()
	defer s.mu.Unlock()

	for n > 0 {
		if s.run == false {
			s.wp = s.mixer.mixrp
			s.run = true
		}

		m := (aBUF-1) - (s.wp - s.mixer.mixrp)

		if m <= 0 {
			s.run = true
			s.cond.Wait()
			continue
		}

		if m > n {
			m = n
		}

		//s.mixer.Lock()
		for i := 0; i < m; i++ {
			for j := 0; j < aCHAN; j++ {
				s.mixer.mixbuf[s.wp%aBUF][j] += s16(p)
				p = p[2:]
			}
			s.wp++
		}
		//s.mixer.Unlock()
		
		n -= m
	}

	if (s.wp - s.mixer.mixrp) >= aDELAY {
		s.run = true
		s.cond.Wait()
	}

	return wc, nil
}

func (s *Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used == false {
		return fmt.Errorf("closed")
	}

	s.used = false

	m := s.mixer

	m.Lock()
	defer m.Unlock()

	delete(m.streams, s.id)

	return nil
}

type Mixer struct {
	sync.Mutex
	audio    Sink
	mixrp    int
	mixbuf   [aBUF][aCHAN]int32
	nstreams int
	streams  map[int]*Stream
	quit     chan bool
}

func (m Mixer) String() string {
	return fmt.Sprintf("Mixer %q mixrp %d", m.audio, m.mixrp)
}

func NewMixer(audio Sink) (*Mixer) {
	m := &Mixer{
		audio:    audio,
		mixrp:    0,
		nstreams: 0,
		streams:  make(map[int]*Stream),
		quit:     make(chan bool, 1),
	}

	go m.mix()

	return m
}

func (m *Mixer) Stream() io.WriteCloser {
	s := &Stream{
		mixer: m,
		used:  true,
	}

	s.cond = sync.NewCond(&s.mu)

	m.Lock()
	defer m.Unlock()

	m.nstreams++
	s.id = m.nstreams

	m.streams[m.nstreams] = s

	return s
}

func (m *Mixer) Close() error {
	m.Lock()
	defer m.Unlock()
	for i, s := range m.streams {
		s.used = false
		delete(m.streams, i)
	}
	close(m.quit)
	return nil
}

func (m *Mixer) mix() {
	var sweep bool
	var mb, n int

	obuf := make([]byte, aBUF*aCHAN*2)
	sweep = false
	for {
		select {
		case <-m.quit:
			break
		default:
			mb = aBUF
			m.Lock()
			for _, s := range m.streams {
				s.mu.Lock()
				//dbg("mix() check %s", s)
				if s.run {
					n = s.wp - m.mixrp
					if n <= 0 && (s.used == false || sweep == true) {
						s.run = false
					} else if n < mb {
						mb = n
					}
					if n < aDELAY {
						//dbg("mix() waking up %s", s)
						s.cond.Signal()
					}
				}
				s.mu.Unlock()
			}
			m.Unlock()

			mb = mb % aBUF

			if mb == 0 {
				ms := int64(100)
				if m.audio.Ready() {
					if sweep {
						m.audio.Idle()
					} else {
						ms = m.audio.Length()
						if ms > 0 {
							ms *= 800
							ms /= aFREQ * aCHAN * 2
						} else {
							ms = 4
						}
					}
					sweep = true
				}
				time.Sleep(time.Duration(ms) * time.Millisecond)
				continue
			}

			sweep = false

			if !m.audio.Ready() {
				if _, err := m.audio.Write([]byte{}); err != nil {
				dbg("mix(): %s", err)
				time.Sleep(1 * time.Second)
				continue
				}
			}

			p := obuf
			nb := 0
			//m.Lock()
			for i := 0; i < mb; i++ {
				for j := 0; j < aCHAN; j++ {
					v := clip16(m.mixbuf[m.mixrp%aBUF][j])
					m.mixbuf[m.mixrp%aBUF][j] = 0
					p[0] = byte(v & 0xFF)
					p[1] = byte(v >> 8)
					p = p[2:]
					nb += 2
				}
				m.mixrp++
			}
			//m.Unlock()

			m.audio.Write(obuf[:nb])
		}
	}
}

func s16(p []byte) (v int32) {
	v = int32(p[0])<<(2*8) | int32(p[1])<<(3*8)
	v = v >> (2 * 8)
	return
}

func clip16(v int32) int32 {
	if v > 0x7FFF {
		return 0x7FFF
	}
	if v < -0x8000 {
		return -0x8000
	}
	return v
}
