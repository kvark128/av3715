package player

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kvark128/OnlineLibrary/internal/config"
	"github.com/kvark128/OnlineLibrary/internal/connect"
	"github.com/kvark128/OnlineLibrary/internal/flag"
	"github.com/kvark128/OnlineLibrary/internal/gui"
	"github.com/kvark128/OnlineLibrary/internal/lkf"
	"github.com/kvark128/OnlineLibrary/internal/util"
	daisy "github.com/kvark128/daisyonline"
)

// Supported mime types of content
const (
	MP3_FORMAT = "audio/mpeg"
	LKF_FORMAT = "audio/x-lkf"
)

const (
	DEFAULT_SPEED = 1.0
	MIN_SPEED     = 0.5
	MAX_SPEED     = 3.0
)

type Player struct {
	sync.Mutex
	playList []daisy.Resource
	bookID   string
	bookName string
	playing  *flag.Flag
	wg       *sync.WaitGroup
	pause    bool
	trk      *track
	speed    float64
	fragment int
	offset   time.Duration
}

func NewPlayer(bookID, bookName string, resources []daisy.Resource) *Player {
	p := &Player{
		playing:  new(flag.Flag),
		wg:       new(sync.WaitGroup),
		bookID:   bookID,
		bookName: bookName,
		speed:    DEFAULT_SPEED,
	}

	// The player supports only LKF and MP3 formats. Unsupported resources must not be uploaded to the player
	for _, r := range resources {
		if r.MimeType == LKF_FORMAT || r.MimeType == MP3_FORMAT {
			p.playList = append(p.playList, r)
		}
	}

	return p
}

func (p *Player) ChangeSpeed(offset float64) {
	if p == nil {
		return
	}
	p.Lock()
	newSpeed := p.speed + offset
	p.Unlock()
	p.SetSpeed(newSpeed)
}

func (p *Player) SetSpeed(speed float64) {
	if p == nil {
		return
	}

	p.Lock()
	switch {
	case speed < MIN_SPEED:
		p.speed = MIN_SPEED
	case speed > MAX_SPEED:
		p.speed = MAX_SPEED
	default:
		p.speed = speed
	}
	if p.trk != nil {
		p.trk.setSpeed(p.speed)
	}
	p.Unlock()
}

func (p *Player) ChangeTrack(offset int) {
	if p == nil {
		return
	}
	p.Lock()
	newFragment := p.fragment + offset
	p.Unlock()
	p.SetTrack(newFragment)
}

func (p *Player) SetTrack(fragment int) {
	if p == nil {
		return
	}
	p.Lock()
	switch {
	case fragment < 0:
		p.fragment = 0
	case fragment >= len(p.playList):
		p.Unlock()
		return
	default:
		p.fragment = fragment
	}
	p.Unlock()
	if p.playing.IsSet() {
		p.Play()
	}
}

func (p *Player) ChangeVolume(offset int) {
	if p == nil {
		return
	}

	p.Lock()
	defer p.Unlock()
	if p.trk == nil {
		return
	}

	l, r := p.trk.wp.GetVolume()
	newOffset := offset * 4096
	newL := int(l) + newOffset
	newR := int(r) + newOffset

	if newL < 0 {
		newL = 0
	}
	if newL > 0xffff {
		newL = 0xffff
	}

	if newR < 0 {
		newR = 0
	}
	if newR > 0xffff {
		newR = 0xffff
	}

	p.trk.wp.SetVolume(uint16(newL), uint16(newR))
}

func (p *Player) Rewind(offset time.Duration) {
	if p == nil {
		return
	}
	p.Lock()
	defer p.Unlock()
	if !p.playing.IsSet() {
		p.offset = offset
		return
	}
	if p.trk != nil {
		if err := p.trk.rewind(offset); err != nil {
			log.Printf("rewind: %v", err)
		}
	}
}

func (p *Player) Play() {
	if p == nil {
		return
	}
	p.Stop()
	p.Lock()
	p.pause = false
	p.wg.Add(1)
	go p.start(p.fragment)
	p.Unlock()
}

func (p *Player) PlayPause() {
	if p == nil {
		return
	}
	if !p.playing.IsSet() {
		p.Play()
	} else {
		p.Lock()
		if p.trk != nil {
			p.pause = !p.pause
			p.trk.pause(p.pause)
		}
		p.Unlock()
	}
}

func (p *Player) Stop() {
	if p == nil {
		return
	}

	p.playing.Clear()
	p.Lock()
	elapsedTime := p.offset
	if p.trk != nil {
		elapsedTime = p.trk.getElapsedTime()
		p.trk.stop()
	}
	service, _, _ := config.Conf.Services.CurrentService()
	service.RecentBooks.SetBook(p.bookID, p.bookName, p.fragment, elapsedTime)
	p.Unlock()
	p.wg.Wait()
}

func (p *Player) start(startFragment int) {
	defer p.wg.Done()
	defer p.playing.Clear()
	p.playing.Set()

	for i, r := range p.playList[startFragment:] {
		var src io.ReadCloser
		var uri string
		var err error

		uri = filepath.Join(config.UserData(), util.ReplaceProhibitCharacters(p.bookName), r.LocalURI)
		if info, e := os.Stat(uri); e == nil {
			if !info.IsDir() && info.Size() == r.Size {
				// track already exist
				src, _ = os.Open(uri)
			}
		}

		if src == nil {
			// There is no track on the disc. Trying to get it from the network
			uri = r.URI
			src, err = connect.NewConnection(uri)
			if err != nil {
				log.Printf("Connection creating: %s\n", err)
				break
			}
		}

		var mp3 io.Reader
		switch r.MimeType {
		case LKF_FORMAT:
			mp3 = lkf.NewLKFReader(src)
		case MP3_FORMAT:
			mp3 = src
		default:
			panic("Unsupported MimeType")
		}

		p.Lock()
		speed := p.speed
		offset := p.offset
		p.offset = 0
		p.Unlock()

		trk, err := newTrack(mp3, speed, r.Size)
		if err != nil {
			log.Printf("new track for %v: %v", uri, err)
			continue
		}

		if err := trk.rewind(offset); err != nil {
			log.Printf("track rewind: %v", err)
			continue
		}

		if !p.playing.IsSet() {
			src.Close()
			break
		}
		currentFragment := startFragment + i

		p.Lock()
		p.trk = trk
		p.fragment = currentFragment
		p.Unlock()

		log.Printf("playing %s: %s", uri, r.MimeType)
		gui.SetFragments(currentFragment, len(p.playList))
		trk.play()
		src.Close()
		log.Printf("stopping %s: %s", uri, r.MimeType)

		p.Lock()
		p.trk = nil
		p.Unlock()

		if !p.playing.IsSet() {
			break
		}
	}
}
