package main

import (
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	MaxThread = 5
	CacheSize = 1024
)

type Status struct {
	Downloaded int64
	Speeds     int64
}

type Block struct {
	Begin int64
	End   int64
}

type File struct {
	Url    string
	Size   int64
	Stream *os.File

	BlockList []Block

	onStart  func()
	onPause  func()
	onResume func()
	onFinish func()
	onError  func(int, error)

	paused bool
	status Status
}

func New(url string, file *os.File) (*File, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return &File{
		Url:    url,
		Size:   resp.ContentLength,
		Stream: file,
	}, nil
}

func (f *File) Start() {
	go func() {
		if f.Size <= 0 {
			f.BlockList = append(f.BlockList, Block{0, -1})
		} else {
			blockSize := f.Size / int64(MaxThread)
			var begin int64
			for i := 0; i < MaxThread; i++ {
				var end = (int64(i) + 1) * blockSize
				f.BlockList = append(f.BlockList, Block{begin, end})
				begin = end + 1
			}
			f.BlockList[MaxThread-1].End += f.Size - f.BlockList[MaxThread-1].End
		}

		go f.onStart()
		err := f.download()
		if err != nil {
			f.onError(0, err)
			return
		}
	}()
}

func (f *File) download() error {
	f.startGetSpeeds()

	ok := make(chan bool, MaxThread)
	for i := range f.BlockList {
		go func(id int) {
			defer func() {
				ok <- true
			}()

			for {
				err := f.downloadBlock(id)
				if err != nil {
					f.onError(0, err)
					continue
				}
				break
			}
		}(i)
	}

	for i := 0; i < MaxThread; i++ {
		<-ok
	}
	if f.paused {
		f.onPause()
		return nil
	}
	f.paused = true
	f.onFinish()

	return nil
}

func (f *File) downloadBlock(id int) error {
	request, err := http.NewRequest("GET", f.Url, nil)
	if err != nil {
		return err
	}
	begin := f.BlockList[id].Begin
	end := f.BlockList[id].End
	if end != -1 {
		request.Header.Set(
			"Range",
			"bytes="+strconv.FormatInt(begin, 10)+"-"+strconv.FormatInt(end, 10),
		)
	}

	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var buf = make([]byte, CacheSize)
	for {
		if f.paused {
			return nil
		}

		n, e := resp.Body.Read(buf)

		bufSize := int64(len(buf[:n]))
		if end != -1 {
			needSize := f.BlockList[id].End + 1 - f.BlockList[id].Begin
			if bufSize > needSize {
				bufSize = needSize
				n = int(needSize)
				e = io.EOF
			}
		}
		f.Stream.WriteAt(buf[:n], f.BlockList[id].Begin)

		f.status.Downloaded += bufSize
		f.BlockList[id].Begin += bufSize

		if e != nil {
			if e == io.EOF {
				return nil
			}
			return e
		}
	}
}

func (f *File) Pause() {
	f.paused = true
}

func (f *File) Resume() {
	f.paused = false
	go func() {
		if f.BlockList == nil {
			f.onError(0, errors.New("BlockList == nil, can not get block info"))
			return
		}

		f.onResume()
		err := f.download()
		if err != nil {
			f.onError(0, err)
			return
		}
	}()
}

func (f *File) startGetSpeeds() {
	go func() {
		var old = f.status.Downloaded
		for {
			if f.paused {
				f.status.Speeds = 0
				return
			}
			time.Sleep(time.Second * 1)
			f.status.Speeds = f.status.Downloaded - old
			old = f.status.Downloaded
		}
	}()
}

func main() {
	destination, err := os.Create("/tmp/" + os.Args[2])
	if err != nil {
		log.Println(err)
	}
	defer destination.Close()

	file, err := New(os.Args[1], destination)
	if err != nil {
		log.Println(err)
	}

	var exit = make(chan bool)
	var resume = make(chan bool)
	var pause bool
	var wg sync.WaitGroup
	wg.Add(1)
	file.onStart = func() {
		log.Println("download started")
		format := "\033[2K\r%v/%v [%s] %v byte/s %v"
		for {
			var i = float64(file.status.Downloaded) / float64(file.Size) * 50
			h := strings.Repeat("=", int(i)) + strings.Repeat(" ", 50-int(i))

			select {
			case <-exit:
				log.Printf(format, file.status.Downloaded, file.Size, h, 0, "[FINISH]")
				log.Println("\ndownload finished")
				wg.Done()
			default:
				if !pause {
					time.Sleep(time.Second * 1)
					log.Printf(format, file.status.Downloaded, file.Size, h, file.status.Speeds, "[DOWNLOADING]")
					os.Stdout.Sync()
				} else {
					log.Printf(format, file.status.Downloaded, file.Size, h, 0, "[PAUSE]")
					os.Stdout.Sync()
					<-resume
					pause = false
				}
			}
		}
	}

	file.onPause = func() {
		pause = true
	}

	file.onError = func(errCode int, err error) {
		log.Println(errCode, err)
	}

	file.onResume = func() {
		resume <- true
	}

	file.onFinish = func() {
		exit <- true
	}

	file.onError = func(errCode int, err error) {
		log.Println(errCode, err)
	}

	log.Printf("%+v\n", file)

	file.Start()
	time.Sleep(time.Second * 2)
	file.Pause()
	time.Sleep(time.Second * 3)
	file.Resume()
	wg.Wait()

}
