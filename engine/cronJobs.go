package engine

import (
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/anacrolix/torrent"
	"github.com/asdine/storm"
	Settings "github.com/deranjer/goTorrent/settings"
	Storage "github.com/deranjer/goTorrent/storage"
	"github.com/mmcdole/gofeed"
	"github.com/robfig/cron"
	"github.com/sirupsen/logrus"
)

//InitializeCronEngine initializes and starts the cron engine so we can add tasks as needed, returns pointer to the engine
func InitializeCronEngine() *cron.Cron {
	c := cron.New()
	c.Start()
	return c
}

//CheckTorrentWatchFolder adds torrents from a watch folder //TODO see if you can use filepath.Abs instead of changing directory
func CheckTorrentWatchFolder(c *cron.Cron, db *storm.DB, tclient *torrent.Client, torrentLocalStorage Storage.TorrentLocal, config Settings.FullClientSettings, torrentQueues Storage.TorrentQueues) {
	c.AddFunc("@every 5m", func() {
		Logger.WithFields(logrus.Fields{"Watch Folder": config.TorrentWatchFolder}).Info("Running the watch folder cron job")
		torrentFiles, err := ioutil.ReadDir(config.TorrentWatchFolder)
		if err != nil {
			Logger.WithFields(logrus.Fields{"Folder": config.TorrentWatchFolder, "Error": err}).Error("Unable to read from the torrent upload folder")
			return
		}
		for _, file := range torrentFiles {
			if filepath.Ext(file.Name()) != ".torrent" {
				Logger.WithFields(logrus.Fields{"File": file.Name(), "error": err}).Error("Not a torrent file..")
				continue
			} else {
				fullFilePath := filepath.Join(config.TorrentWatchFolder, file.Name())
				fullFilePathAbs, err := filepath.Abs(fullFilePath)

				fullNewFilePath := filepath.Join(config.TFileUploadFolder, file.Name())
				fullNewFilePathAbs, err := filepath.Abs(fullNewFilePath)
				Logger.WithFields(logrus.Fields{"Name": file.Name(), "FullFilePath": fullFilePathAbs, "newFullFilePath": fullNewFilePathAbs}).Info("Attempting to add the following file... and copy to")
				CopyFile(fullFilePathAbs, fullNewFilePathAbs)
				clientTorrent, err := tclient.AddTorrentFromFile(fullNewFilePathAbs)
				if err != nil {
					Logger.WithFields(logrus.Fields{"err": err, "Torrent": file.Name()}).Warn("Unable to add torrent to torrent client!")
					continue
				}

				os.Remove(fullFilePathAbs) //delete the torrent after adding it and copying it over
				Logger.WithFields(logrus.Fields{"Source Folder": fullFilePathAbs, "Destination Folder": fullNewFilePathAbs, "Torrent": file.Name()}).Info("Added torrent from watch folder, and moved torrent file")
				AddTorrent(clientTorrent, torrentLocalStorage, db, "file", fullNewFilePathAbs, config.DefaultMoveFolder, "default", config)

			}
		}
	})
}

//CheckTorrentsCron runs a upload ratio check, a queue check (essentially anything that should not be frontend dependent)
func CheckTorrentsCron(c *cron.Cron, db *storm.DB, tclient *torrent.Client, config Settings.FullClientSettings) {
	c.AddFunc("@every 30s", func() {
		Logger.Debug("Running a torrent Ratio and Queue Check")
		torrentLocalArray := Storage.FetchAllStoredTorrents(db)
		torrentQueues := Storage.FetchQueues(db)
		for _, singleTorrentFromStorage := range torrentLocalArray {
			var singleTorrent *torrent.Torrent
			for _, liveTorrent := range tclient.Torrents() { //matching the torrent from storage to the live torrent
				if singleTorrentFromStorage.Hash == liveTorrent.InfoHash().String() {
					singleTorrent = liveTorrent
				}
			}
			calculatedCompletedSize := CalculateCompletedSize(singleTorrentFromStorage, singleTorrent)
			bytesCompleted := CalculateCompletedSize(singleTorrentFromStorage, singleTorrent)
			if float64(singleTorrentFromStorage.UploadedBytes)/float64(bytesCompleted) >= config.SeedRatioStop && singleTorrentFromStorage.TorrentUploadLimit == true { //If storage shows torrent stopped or if it is over the seeding ratio AND is under the global limit
				Logger.WithFields(logrus.Fields{"Action: Stopping torrent due to seed Ratio": singleTorrentFromStorage.TorrentName}).Info()
				StopTorrent(singleTorrent, singleTorrentFromStorage, db)
			}
			if len(torrentQueues.ActiveTorrents) < config.MaxActiveTorrents && singleTorrentFromStorage.TorrentStatus == "Queued" {
				Logger.WithFields(logrus.Fields{"Action: Adding Torrent to Active Queue": singleTorrentFromStorage.TorrentName}).Info()
				AddTorrentToActive(singleTorrentFromStorage, singleTorrent, db)
			}
			if (calculatedCompletedSize == singleTorrentFromStorage.TorrentSize) && (singleTorrentFromStorage.TorrentMoved == false) { //if we are done downloading and haven't moved torrent yet
				Logger.WithFields(logrus.Fields{"singleTorrent": singleTorrentFromStorage.TorrentName}).Info("Torrent Completed, moving...")
				tStorage := Storage.FetchTorrentFromStorage(db, singleTorrent.InfoHash().String()) //Todo... find a better way to do this in the go-routine currently just to make sure it doesn't trigger multiple times
				tStorage.TorrentMoved = true
				Storage.UpdateStorageTick(db, tStorage)
				go func() { //moving torrent in separate go-routine then verifying that the data is still there and correct
					err := MoveAndLeaveSymlink(config, singleTorrent.InfoHash().String(), db, false, "") //can take some time to move file so running this in another thread TODO make this a goroutine and skip this block if the routine is still running
					if err != nil {                                                                      //If we fail, print the error and attempt a retry
						Logger.WithFields(logrus.Fields{"singleTorrent": singleTorrentFromStorage.TorrentName, "error": err}).Error("Failed to move Torrent!")
						VerifyData(singleTorrent)
						tStorage.TorrentMoved = false
						Storage.UpdateStorageTick(db, tStorage)
					}
				}()
			}

		}
		ValidateQueues(db, config, tclient) //Ensure we don't have too many in activeQueue
	})
}

//RefreshRSSCron refreshes all of the RSS feeds on an hourly basis
func RefreshRSSCron(c *cron.Cron, db *storm.DB, tclient *torrent.Client, torrentLocalStorage Storage.TorrentLocal, config Settings.FullClientSettings, torrentQueues Storage.TorrentQueues) {
	c.AddFunc("@hourly", func() {
		torrentHashHistory := Storage.FetchHashHistory(db)
		RSSFeedStore := Storage.FetchRSSFeeds(db)
		singleRSSTorrent := Storage.SingleRSSTorrent{}
		newFeedStore := Storage.RSSFeedStore{ID: RSSFeedStore.ID} //creating a new feed store just using old one to parse for new torrents
		fp := gofeed.NewParser()
		for _, singleFeed := range RSSFeedStore.RSSFeeds {
			feed, err := fp.ParseURL(singleFeed.URL)
			if err != nil {
				Logger.WithFields(logrus.Fields{"err": err, "url": singleFeed.URL}).Error("Failed to parse RSS URL")
			}
			for _, RSSTorrent := range feed.Items {
				Logger.WithFields(logrus.Fields{"Torrent": RSSTorrent.Title}).Info("Found new torrent")
				singleRSSTorrent.Link = RSSTorrent.Link
				singleRSSTorrent.Title = RSSTorrent.Title
				singleRSSTorrent.PubDate = RSSTorrent.Published
				for _, hash := range torrentHashHistory.HashList {
					linkHash := singleRSSTorrent.Link[20:60] //cutting the infohash out of the link
					if linkHash == hash {
						Logger.WithFields(logrus.Fields{"Torrent": RSSTorrent.Title}).Warn("Torrent already added for this RSS item, skipping torrent")
					}
				}
				clientTorrent, err := tclient.AddMagnet(RSSTorrent.Link)
				if err != nil {
					Logger.WithFields(logrus.Fields{"err": err, "Torrent": RSSTorrent.Title}).Warn("Unable to add torrent to torrent client!")
					break //break out of the loop entirely for this message since we hit an error
				}
				AddTorrent(clientTorrent, torrentLocalStorage, db, "magnet", "", config.DefaultMoveFolder, "RSS", config) //TODO let user specify torrent default storage location and let change on fly
				singleFeed.Torrents = append(singleFeed.Torrents, singleRSSTorrent)

			}
			newFeedStore.RSSFeeds = append(newFeedStore.RSSFeeds, singleFeed)
		}
		Storage.UpdateRSSFeeds(db, newFeedStore) //Calling this to fully update storage will all rss feeds
	})

}

//LogCronStatus prints out the status of the cron jobs to the log
func LogCronStatus(c *cron.Cron) { //TODO add a cron to inspect cron jobs and log the outputs

}
