package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/adrg/xdg"
)

type DanbooruJson []struct {
	ID       int
	MD5      string
	File_EXT string
	File_URL string
}

type eforosData struct {
	goodCsums   []string
	goodCsumsFd *os.File
	goodURLsFd  *os.File
	badCsums    []string
	badCsumsFd  *os.File
	tmpFd       *os.File
}

func copyImg(statusChan chan bool, img []byte, md5 string, ext string) {
	imgCopyFdChan := make(chan *os.File)
	go openFile(imgCopyFdChan, md5+"."+ext)
	imgCopyFd := <-imgCopyFdChan
	defer imgCopyFd.Close()

	imgCopyFd.Write(img)
	statusChan <- true
}

func eforosDataInit(edChan chan *eforosData) {
	var ed eforosData
	goodCsumsPathChan := make(chan string)
	badCsumsPathChan := make(chan string)
	goodURLsPathChan := make(chan string)

	go xdgDatfile(goodCsumsPathChan, "eforos/good_csums")
	go xdgDatfile(badCsumsPathChan, "eforos/bad_csums")
	go xdgDatfile(goodURLsPathChan, "eforos/good_urls")

	goodCsumsFdChan := make(chan *os.File)
	badCsumsFdChan := make(chan *os.File)
	goodURLsFdChan := make(chan *os.File)

	go openFile(goodCsumsFdChan, <-goodCsumsPathChan)
	go openFile(badCsumsFdChan, <-badCsumsPathChan)
	go openFile(goodURLsFdChan, <-goodURLsPathChan)

	goodCsumsChan := make(chan []string)
	ed.goodCsumsFd = <-goodCsumsFdChan
	go stringify(goodCsumsChan, ed.goodCsumsFd)

	badCsumsChan := make(chan []string)
	ed.badCsumsFd = <-badCsumsFdChan
	go stringify(badCsumsChan, ed.badCsumsFd)

	ed.goodURLsFd = <-goodURLsFdChan
	ed.goodCsums = <-goodCsumsChan
	ed.badCsums = <-badCsumsChan

	edChan <- &ed
}

func errCheck(err error) {
	if err != nil {
		panic(err)
	}
}

func fdWriteStr(statusChan chan bool, fd *os.File, input string) {
	_, err := fd.WriteString(input)
	errCheck(err)
	statusChan <- true
}

func httpDataHandler(dataChan chan []byte, url string) {
	var resp *http.Response
	statusOk := false

	for i := 0; statusOk == false; i++ {
		time.Sleep(time.Duration(i) * time.Second)
		var err error
		resp, err = http.Get(url)
		errCheck(err)
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			statusOk = true
		}
	}

	data, err := ioutil.ReadAll(resp.Body)
	errCheck(err)

	dataChan <- data
}

func createTmp(tmpChan chan *os.File, tmpFd *os.File) {
	tmpFd, err := os.CreateTemp("", "eforos")
	errCheck(err)
	tmpChan <- tmpFd
}

func csumsCheck(csum string, goodCsums []string, badCsums []string) bool {
	if slices.Contains(goodCsums, csum) || slices.Contains(badCsums, csum) {
		return true
	}

	return false
}

func openFile(fdChan chan *os.File, path string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	errCheck(err)

	fdChan <- f
}

func stringify(stringsChan chan []string, fd *os.File) {
	var strings []string
	scanner := bufio.NewScanner(fd)
	for scanner.Scan() {
		strings = append(strings, scanner.Text())
	}

	stringsChan <- strings
}

func xdgDatfile(pathChan chan string, relPath string) {
	path, err := xdg.DataFile(relPath)
	errCheck(err)

	pathChan <- path
}

func eforosdrv(ed *eforosData) {
	for {
		danbooruReqChan := make(chan []byte)
		var danbooru DanbooruJson

		// resp, err := http.Get("https://testbooru.donmai.us/posts.json?tags=yuri+order:random+limit:1")
		go httpDataHandler(danbooruReqChan, "https://danbooru.donmai.us/posts.json?tags=yuri+order:random+rating:sensitive")

		err := json.Unmarshal(<-danbooruReqChan, &danbooru)
		errCheck(err)

		for i := 0; i < len(danbooru); i++ {
			imgCsum := danbooru[i].MD5
			if csumsCheck(imgCsum, ed.goodCsums, ed.badCsums) != false || danbooru[i].File_URL == "" {
				continue
			}

			imgChan := make(chan []byte)
			go httpDataHandler(imgChan, danbooru[i].File_URL)

			tmpChan := make(chan *os.File)
			go createTmp(tmpChan, ed.tmpFd)

			img := <-imgChan
			ed.tmpFd = <-tmpChan
			_, err = ed.tmpFd.Write(img)
			errCheck(err)

			tmpName := ed.tmpFd.Name()

			/* let's just call loupe directly for now */
			cmd := exec.Command("loupe", tmpName)
			errCheck(cmd.Start())

			gbChan := make(chan string)
			go func() {
				var gb string
				for gb != "b" && gb != "g" {
					fmt.Printf("[g]ood or [b]ad?: ")
					fmt.Scan(&gb)
				}
				gbChan <- gb
			}()

			csumWriteStatus := make(chan bool)
			switch <-gbChan {
			case "b":
				go fdWriteStr(csumWriteStatus, ed.badCsumsFd, imgCsum+"\n")
				ed.badCsums = append(ed.badCsums, imgCsum)
			case "g":
				copyImgStatus := make(chan bool)
				go copyImg(copyImgStatus, img, imgCsum, danbooru[i].File_EXT)
				go fdWriteStr(csumWriteStatus, ed.goodCsumsFd, imgCsum+"\n")

				urlWriteStatus := make(chan bool)
				go fdWriteStr(urlWriteStatus, ed.goodURLsFd, danbooru[i].File_URL+"\n")
				ed.goodCsums = append(ed.goodCsums, imgCsum)
				<-copyImgStatus
				<-urlWriteStatus
			default:
				fmt.Println("uh oh, something went wrong!")
				csumWriteStatus <- true
			}

			errCheck(cmd.Process.Kill())
			rmChan := make(chan bool)
			go func() {
				os.Remove(tmpName)
				rmChan <- true
			}()

			ed.tmpFd.Close()
			<-csumWriteStatus
			<-rmChan
		}
	}
}

func main() {
	edChan := make(chan *eforosData)
	go eforosDataInit(edChan)

	sigs := make(chan os.Signal)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	ed := <-edChan
	defer ed.goodCsumsFd.Close()
	defer ed.badCsumsFd.Close()
	defer ed.goodURLsFd.Close()
	defer ed.tmpFd.Close()

	go eforosdrv(ed)
	<-sigs
	os.Remove(ed.tmpFd.Name())
}
