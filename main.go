// Console player for rockradio.com online radio station.
// Made just for fun and personal comfort (avoid registration and advertising).
package main

import (
	"fmt"
	"os"
	"log"
	"os/user"
	"github.com/BurntSushi/xgbutil"
	"io/ioutil"
	"encoding/json"
	"github.com/BurntSushi/xgbutil/keybind"
	"github.com/BurntSushi/xgbutil/xevent"
	"sync"
	"flag"
	"os/signal"
	"syscall"
	"github.com/fsnotify/fsnotify"
	"time"
	"net/http"
	"regexp"
	"github.com/PuerkitoBio/goquery"
	"strconv"
	"path"
	"bufio"
	"sort"
	"strings"

	mp3 "github.com/koykov/mp3lib"
)

const (
	STATUS_PLAY   = 0x100
	STATUS_PAUSE  = 0x200
	STATUS_STOP   = 0x300
)

// JSON types
type Hotkey struct {
	Key					string `json:"key"`
	Desc				string `json:"desc"`
}

type ChannelChunk struct {
	Id					uint64 `json:"channel_id"`
	Expiry				string `json:"expires_on"`
	Length				float64
	Tracks				[]ChannelChunk__Tracks `json:"tracks"`
}

type ChannelChunk__Tracks struct {
	Id					uint64 `json:"id"`
	Artist				string `json:"display_artist"`
	Title				string `json:"display_title"`
	Album				string `json:"release"`
	AlbumDate			string `json:"release_date"`
	Content				ChannelChunk__Tracks__Content `json:"content"`
}

type ChannelChunk__Tracks__Content struct {
	Length				float64 `json:"length"`
	Assets				[]ChannelChunk__Tracks__Content__Assets `json:"assets"`
}

type ChannelChunk__Tracks__Content__Assets struct {
	Url					string `json:"url"`
}

// General types
type RockRadioPlayer struct {
	Channels			map[uint64]RockRadioPlayerChannel
	CurrentChunk		ChannelChunk
	CurrentTrack		ChannelChunk__Tracks__Content__Assets
	CurrentChannel		uint64
	AudioToken			string
	Status				uint64
	NextFetch			uint64
}

type RockRadioPlayerChannel struct {
	Id					uint64 `json:"Id"`
	Title				string `json:"Title"`
}

var rr RockRadioPlayer
var verbose bool

func init() {
	// Check (and create if needed) configuration directory.
	configDir := GetConfigDir()
	_, err := os.Stat(configDir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(configDir, 0755); err != nil {
			log.Fatal("Cannot create configuration diectory.")
		}
	}
	// Check (and create) hotkeys configuration file.
	hotkeyConfig := GetHotkeyConfig()
	_, err = os.Stat(hotkeyConfig)
	if os.IsNotExist(err) {
		// For possible keys see https://github.com/BurntSushi/xgbutil/blob/master/keybind/keysymdef.go
		// Unfortunately, there isn't possibility to specify a key combination, only one key may be used.
		PutToFile(hotkeyConfig, `[
	{
		"key": "Pause",
		"desc": "Play/pause."
	}
]`)
		Debug("create default config file - %s", hotkeyConfig)
	}
	// Check (and create if needed) cache directory.
	cacheDir := GetCacheDir()
	_, err = os.Stat(cacheDir)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(cacheDir, 0755); err != nil {
			log.Fatal("Cannot create cache diectory.")
		}
	}
}

func main() {
	var wg sync.WaitGroup

	// Parse CLI options.
	channelPtr := flag.Int("c", 0, "Channel ID.")
	verbosePtr := flag.Bool("verbose", false, "Display debug messages.")
	flag.Parse()

	verbose = *verbosePtr

	// First fetch unique AudioToken. It have no sense to continue without it.
	rr.FetchAudioToken()

	// Make goroutine for final cleanup callback.
	wg.Add(1)
	c := make(chan os.Signal, 2)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		defer wg.Done()
		<-c
		Cleanup()
		os.Exit(1)
	}()

	// Initialize keybinding.
	X, err := xgbutil.NewConn()
	if err != nil {
		log.Fatal(err)
	}
	keybind.Initialize(X)

	hotkeyConfig := GetHotkeyConfig()
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	err = watcher.Add(hotkeyConfig)
	if err != nil {
		log.Println(err)
	}

	// Keybinding goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case ev := <-watcher.Events:
				log.Println(ev)
				err := bindall(hotkeyConfig, X)
				if err != nil {
					log.Println(err)
					continue
				}

			case err := <-watcher.Errors:
				log.Println("error:", err)
			}
		}
	}()
	err = bindall(hotkeyConfig, X)
	if err != nil {
		log.Panicln(err)
	}

	// Event handling goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		xevent.Main(X)
	}()

	// Cache check.
	cacheFile := GetCacheDir() + string(os.PathSeparator) + "data.json"
	needRegenerate := false
	fi, err := os.Stat(cacheFile)
	if err != nil {
		if os.IsNotExist(err) {
			needRegenerate = true
			Debug("Cache file %s doesn't exists, need generate.", cacheFile)
		} else {
			log.Fatal("Error when reading cache file: %s", err.Error())
		}
	}
	if !needRegenerate {
		now := time.Now()
		mtime := fi.ModTime()
		diff := now.Sub(mtime)
		needRegenerate = diff.Seconds() > 7*24*3600
		if needRegenerate {
			Debug("Cache file %s is deprecated, need regenerate.", cacheFile)
		}
	}
	if !needRegenerate {
		// Read channels and groups from the cache.
		raw, err := ioutil.ReadFile(cacheFile)
		if err != nil {
			log.Fatal("Error reading cache file: %s", err.Error())
		}
		rr.Channels = make(map[uint64]RockRadioPlayerChannel)
		json.Unmarshal(raw, &rr.Channels)
		Debug("Cache hit, reading file %s", cacheFile)
	} else {
		// Fetch channels and groups from 101.ru
		rr.FetchChannels()

		b, err := json.Marshal(rr.Channels)
		if err != nil {
			log.Fatal(err.Error())
		}

		PutToFile(cacheFile, string(b))
		Debug("Write channels data to cache file %s", cacheFile)
	}

	// Choose channel.
	if *channelPtr == 0 {
		reader := bufio.NewReader(os.Stdin)

		cls := make([]string, len(rr.Channels))
		for _, c := range rr.Channels {
			cls = append(cls, fmt.Sprintf("%d - %s\n", c.Id, c.Title))
		}
		sort.Strings(cls)
		fmt.Println("Choose channel:")
		for _, g := range cls {
			fmt.Print(g)
		}
		fmt.Print("\nChannel: ")
		channelIndex, _ := reader.ReadString('\n')
		rr.CurrentChannel, _ = strconv.ParseUint(strings.Trim(channelIndex, "\n"), 10, 64)
	} else {
		rr.CurrentChannel = uint64(*channelPtr)
	}
	channel := rr.Channels[rr.CurrentChannel]

	// Playing loop.
	fmt.Printf("\nPlayng: %s\n", channel.Title)
	for true {
		rr.FetchChannelInfo()
		Debug("Fetch remote data %#v", rr.CurrentChunk)
		Debug("Next fetch after %f seconds", rr.CurrentChunk.Length)
		for _, track := range rr.CurrentChunk.Tracks {
			rr.Stop()
			fmt.Printf("%s - %s [%s] - %s\n", track.Artist, track.Title, track.Album, FormatTime(uint64(track.Content.Length)))
			rr.CurrentTrack = track.Content.Assets[0] // @todo check next assets
			go rr.Play()
			rr.Sleep(track.Content.Length)
		}
		// Get fresh audio token after playing the chunk. The old token may expire.
		go rr.FetchAudioToken()
	}

	// Waiting for finishing all goroutines.
	wg.Wait()
}

// Process finish callback.
func Cleanup() {
	rr.Stop()
	Debug("Cleanup sig.")
}

// Returns full path to the config directory.
func GetConfigDir() string {
	usr, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	ps := string(os.PathSeparator)
	return usr.HomeDir + ps + ".config" + ps + "rrply"
}

// Returns full path to the hotkey configuration file.
func GetHotkeyConfig() string {
	ps := string(os.PathSeparator)
	return GetConfigDir() + ps + "hotkey.json"
}

// Returns full path to the cache directory.
func GetCacheDir() string {
	usr, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	ps := string(os.PathSeparator)
	return usr.HomeDir + ps + ".cache" + ps + "rrply"
}

// Create file (if needed) and write contents to him.
func PutToFile(filename string, contents string) {
	if _, err := os.Create(filename); err != nil {
		log.Fatal("Error when file is created: ", err.Error())
	}

	file, err := os.OpenFile(filename, os.O_RDWR, 0644)
	if err != nil {
		log.Fatal("Error when file is created: ", err.Error())
	}
	defer file.Close()
	file.WriteString(contents)
	if err = file.Sync(); err != nil {
		log.Fatal("Error when saving file: ", err.Error())
	}
}

// Parses config file and binds keys to events.
func bindall(hotkeyConfig string, X *xgbutil.XUtil) (err error) {
	config, err := ioutil.ReadFile(hotkeyConfig)
	if err != nil {
		log.Fatal("Could not find config file: ", err.Error())
		return
	}
	hotkeys := []Hotkey{}
	err = json.Unmarshal(config, &hotkeys)
	if err != nil {
		log.Fatal("Could not parse config file: ", err.Error())
		return
	}
	keybind.Detach(X, X.RootWin())
	for _, hotkey := range hotkeys {
		hotkey.attach(X)
	}
	return
}

// Attach callback to the hotkey.
func (hotkey Hotkey) attach(X *xgbutil.XUtil) {
	err := keybind.KeyPressFun(
		func(X *xgbutil.XUtil, e xevent.KeyPressEvent) {
			if (rr.Status == STATUS_STOP || rr.Status == STATUS_PAUSE) {
				go rr.Resume()
			} else {
				go rr.Pause()
			}
		}).Connect(X, X.RootWin(), hotkey.Key, true)
	if err != nil {
		log.Fatalf("Could not bind %s: %s", hotkey.Key, err.Error())
	}
}

// Convert seconds to "mm:ss" time format.
func FormatTime(s uint64) (string) {
	min := s / 60
	sec := s % 60
	return fmt.Sprintf("%d:%d", min, sec)
}

// Print formatted debug message.
func Debug(message string, a ...interface{}) {
	if verbose {
		fmt.Println(fmt.Sprintf("Debug: " + message, a))
	}
}

// Fetch unique audio token from rockradio.com
func (this *RockRadioPlayer) FetchAudioToken() {
	response, err := http.Get("https://www.rockradio.com")
	if err != nil {
		log.Fatal(err)
		return
	}
	defer response.Body.Close()

	source, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Fatal(err)
		return
	}

	re := regexp.MustCompile(`"audio_token"\:"([a-z0-9]+)"`)
	res := re.FindStringSubmatch(string(source))
	if res == nil {
		log.Fatal("Couldn't fetch AudioToken. Exiting.")
	}

	this.AudioToken = res[1]
	Debug("Fetch AudioToken: %s", this.AudioToken)
}

// Fetch channels from rockradio.com
func (this *RockRadioPlayer) FetchChannels() {
	doc, err := goquery.NewDocument("https://www.rockradio.com/")
	if err != nil {
		log.Fatal("Couldn't fetch channels: ", err.Error())
	}

	this.Channels = make(map[uint64]RockRadioPlayerChannel, 0)
	doc.Find("div.submenu.channels li").Each(func(i int, selection *goquery.Selection) {
		id, exists := selection.Attr("data-channel-id")
		title := selection.Find("a").Find("span").Text()
		if exists {
			cid, _ := strconv.ParseUint(path.Base(id), 0, 64)
			this.Channels[cid] = RockRadioPlayerChannel{
				cid, title,
			}
		}
	})
}

// Fetch channel info.
func (this *RockRadioPlayer) FetchChannelInfo() {
	if len(this.AudioToken) == 0 {
		log.Fatal("Unknown AudioTocken. Call self.FetchAudioToken() first.")
	}

	ts := time.Now().UnixNano() / 1000000
	channelUrl := fmt.Sprintf("https://www.rockradio.com/_papi/v1/rockradio/routines/channel/%d?audio_token=%s&_=%d", this.CurrentChannel, this.AudioToken, ts)
	response, err := http.Get(channelUrl)
	if err != nil {
		log.Fatal(err)
		return
	}
	defer response.Body.Close()

	b, err := ioutil.ReadAll(response.Body)
	if err != nil {
		log.Fatal(err)
	}

	var cc ChannelChunk
	err = json.Unmarshal(b, &cc)
	if err != nil {
		log.Fatal(err)
	}
	this.CurrentChunk = cc
	for _, track := range this.CurrentChunk.Tracks {
		this.CurrentChunk.Length += track.Content.Length
	}
}

// Play channel.
func (this *RockRadioPlayer) Play() {
	playUrl := "http:" + this.CurrentTrack.Url
	mp3.PlayProcess(playUrl)
	this.Status = STATUS_PLAY
	Debug("Play sig.")
}

// Pause playing.
func (this *RockRadioPlayer) Pause() {
	mp3.PauseProcess()
	this.Status = STATUS_PAUSE
	Debug("Pause sig.")
}

// Resume playing.
func (this *RockRadioPlayer) Resume() {
	mp3.ResumeProcess()
	this.Status = STATUS_PLAY
	Debug("Resume sig.")
}

// Stop playing.
func (this *RockRadioPlayer) Stop() {
	// Call stop proc twice, just in case.
	mp3.StopProcess()
	mp3.StopProcess()
	this.Status = STATUS_STOP
	Debug("Stop sig.")
}

// Sleep function, freezes duration increment on pause/stop status.
func (this *RockRadioPlayer) Sleep(s float64) {
	var counter float64
	for true {
		time.Sleep(time.Second)
		if this.Status == STATUS_PLAY {
			counter += 1
		}
		if counter >= s {
			break
		}
	}
}
