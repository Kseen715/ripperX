package main

import "net/http"

// Every endpoint this server has, in the order they are worth reading in.
// The mux is built from this table and so is the API document at
// /api/openapi.json, which is what keeps the two honest about each other.
// Adding an endpoint means adding a line here; there is nowhere else to add
// one.
//
// The static files - the pages themselves, the stylesheet and the icon -
// are not endpoints and are not listed.
func (s *server) routes(guard *auth) []route {
	rt := []route{{
		Method: http.MethodGet, Pattern: "/api/status", Handler: s.handleStatus,
		Summary: "What this server is and what it will let you do",
		Desc: "Read once before anything else: it says where images are kept, whether a " +
			"burner is installed, and whether a login is required.",
		Resp: statusResponse{},
	}, {
		Method: http.MethodGet, Pattern: "/api/locales", Handler: s.handleLocales,
		Summary: "The languages this build carries, and which one it prefers",
		Desc: "Readable without signing in, because the sign-in page needs its own words. " +
			"The strings themselves are plain JSON files served from /locales/<code>.json; " +
			"adding a language to a build is adding one of those and nothing else.",
		Resp: localesResponse{},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives", Handler: s.handleDrives,
		Summary: "Every drive, with what it can do and what is in it",
		Desc: "The shallow answer: capabilities and disc, but no filesystem. A drive that " +
			"cannot be asked - because a job has it, or because it has gone away - is " +
			"still listed, with the reason in its error field.",
		Resp: drivesResponse{},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives/{id}", Handler: s.handleDrive,
		Summary: "One drive, including the volume on its disc",
		Desc: "As /api/drives but for one drive, and with the ISO 9660 volume descriptors " +
			"read as well - which costs a seek, and is why the list does not do it.",
		Param: &param{"id", "the drive's id, such as sr0"},
		Resp:  driveView{},
		Other: []status{{http.StatusNotFound, "no drive by that name"}},
	}, {
		Method: http.MethodPost, Pattern: "/api/drives/{id}/refresh", Handler: s.handleDriveRefresh,
		Summary: "Forget what was known about the disc and ask again",
		Desc: "For the case where a disc was swapped in a drive whose firmware did not " +
			"mention it. Everything cached for that drive is dropped.",
		Param: &param{"id", "the drive's id"},
		Resp:  driveView{},
		Other: []status{{http.StatusConflict, "a job has this drive"}},
	}, {
		Method: http.MethodPost, Pattern: "/api/drives/{id}/tray/{action}", Handler: s.handleTray,
		Summary: "Open or close the tray",
		Desc: "The action is eject or load. A server started with -allow-eject=false " +
			"refuses both.",
		Param: &param{"id", "the drive's id, then /tray/eject or /tray/load"},
		Resp:  statusMessage{},
		Other: []status{
			{http.StatusForbidden, "tray control is turned off on this server"},
			{http.StatusConflict, "a job has this drive"},
		},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives/{id}/browse", Handler: s.handleBrowse,
		Summary: "List a directory on the disc",
		Desc: "Reads the disc's directory records where they lie - no mount, no copy. " +
			"Joliet and Rock Ridge names are used where the disc has them, so what comes " +
			"back is what the disc's author saw rather than the 8.3 version.",
		Param: &param{"id", "the drive's id; the directory is the path query parameter, / by default"},
		Resp:  browseResponse{},
		Other: []status{
			{http.StatusConflict, "a job has this drive"},
			{http.StatusUnprocessableEntity, "this disc has no filesystem ripperX can read"},
			{http.StatusNotFound, "no such directory on the disc"},
		},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives/{id}/sizes", Handler: s.handleDirSizes,
		Summary: "How big one or more folders on the disc are",
		Desc: "A file's size is in its own directory record; a folder's is the sum of " +
			"everything under it, which means reading every directory record in the tree. " +
			"That is why a listing does not include it and this endpoint exists. Repeat " +
			"the path parameter to ask about several folders in one pass over the drive.",
		Param: &param{"id", "the drive's id; the directories are the repeated path query parameter"},
		Resp:  dirSizesResponse{},
		Other: []status{
			{http.StatusConflict, "a job has this drive"},
			{http.StatusUnprocessableEntity, "this disc has no filesystem ripperX can read"},
			{http.StatusBadRequest, "too many directories were asked about at once"},
		},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives/{id}/file", Handler: s.handleDiscFile,
		Summary: "One file off the disc",
		Desc: "Streamed straight from its extent, with byte ranges honoured, so a browser " +
			"can scrub through a video without reading the parts it skipped. Add inline=1 " +
			"to play it in the browser rather than download it. A player with no cookie " +
			"may pass an access token as the access_token query parameter. On a " +
			"DVD-Video, title=<n> from /titles takes one title of the disc instead - the " +
			"stretch of its VOBs that title occupies, as one file.",
		Param: &param{"id", "the drive's id; the file is the path query parameter"},
		Type:  "application/octet-stream",
		Other: []status{
			{http.StatusConflict, "a job has this drive"},
			{http.StatusNotFound, "no such file on the disc"},
		},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives/{id}/titles", Handler: s.handleTitles,
		Summary: "What is actually on a video disc",
		Desc: "A DVD's files are not its films: VTS_01_1.VOB and its siblings are a " +
			"gigabyte apiece because the format says so, and what is inside them is " +
			"several titles one after another, each with its own timeline starting at " +
			"zero. This reads the disc's own index - the IFO files - and answers with " +
			"the titles it names, how long each one plays for, and how much of the disc " +
			"it occupies. Play and download take those numbers as title=<n>.",
		Param: &param{"id", "the drive's id"},
		Resp:  dvdDisc{},
		Other: []status{
			{http.StatusNotFound, "this disc is not a DVD-Video"},
			{http.StatusConflict, "a job has this drive"},
			{http.StatusUnprocessableEntity, "this disc's index could not be read"},
		},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives/{id}/hls/index.m3u8", Handler: s.handleHLSPlaylist,
		Summary: "A video on the disc as a playlist a browser can seek in",
		Desc: "For video no browser decodes - a DVD's MPEG-2 above all. It is divided " +
			"into six-second segments this server encodes one at a time, as they are " +
			"asked for, so jumping to the middle costs one segment rather than the whole " +
			"film. Ask for a file with path, or for one of the titles from /titles with " +
			"title=<n>, which is the only way to be right about the length of a DVD. " +
			"Needs ffmpeg on the server.",
		Param: &param{"id", "the drive's id; what to play is the path or title query parameter"},
		Type:  "application/vnd.apple.mpegurl",
		Other: []status{
			{http.StatusNotFound, "no such drive, or this server has no ffmpeg"},
			{http.StatusUnprocessableEntity, "the file does not say how long it is"},
			{http.StatusConflict, "a job has this drive"},
		},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives/{id}/hls/{seg}", Handler: s.handleHLSSegment,
		Summary: "One segment of that playlist, encoded on demand",
		Desc: "Named like 12.ts, and asked for with the same path or title parameter as " +
			"the playlist. A segment already encoded is answered from memory, which is what " +
			"makes scrubbing backwards free; the rest hold the drive for as long as one " +
			"six-second piece takes to read and encode.",
		Param: &param{"id", "the drive's id, then /hls/<segment number>.ts"},
		Type:  "video/mp2t",
		Other: []status{
			{http.StatusNotFound, "no such drive, or this server has no ffmpeg"},
			{http.StatusBadRequest, "that is not a segment number"},
			{http.StatusConflict, "a job has this drive"},
		},
	}, {
		Method: http.MethodGet, Pattern: "/api/internal/source/{nonce}", Handler: s.handleInternalSource,
		Summary: "Not for callers: how this server's own encoder reads a disc",
		Desc: "The encoder has to seek its input and the disc is not a file anywhere, so it " +
			"reads the disc through this server. The name in the path is random, issued " +
			"for one title, forgotten fifteen minutes after it was last used, and refused " +
			"to anything that is not this machine. Nothing else should call it.",
		Param: &param{"nonce", "the name issued for that title"},
		Type:  "application/octet-stream",
		Other: []status{{http.StatusNotFound, "no such name, or the request came from elsewhere"}},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives/{id}/archive", Handler: s.handleDiscArchive,
		Summary: "A directory of the disc as one archive",
		Desc: "Produced as the disc is read - nothing is staged first - so a whole tree can " +
			"be downloaded in one request however large it is. The path query parameter " +
			"defaults to /, and format to zip; /api/formats lists the rest.",
		Param: &param{"id", "the drive's id; the directory is the path query parameter, the format is format"},
		Type:  "application/zip",
		Other: []status{
			{http.StatusConflict, "a job has this drive"},
			{http.StatusBadRequest, "no such archive format"},
		},
	}, {
		Method: http.MethodGet, Pattern: "/api/formats", Handler: s.handleFormats,
		Summary: "The archive formats this server can produce",
		Desc: "Each with a note on when to choose it. rar and 7z are not among them: " +
			"neither has a usable Go writer and rar's format is proprietary, so offering " +
			"them would mean two entries that fail when chosen.",
		Resp: formatsResponse{},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives/{id}/audio/{track}", Handler: s.handleAudio,
		Summary: "One audio track, as a WAV",
		Desc: "The sectors of an audio CD are already PCM, so a WAV header is put in front " +
			"of them and nothing is decoded or re-encoded. Ranges are honoured, so seeking " +
			"in the browser seeks the laser. The track is named like 3.wav.",
		Param: &param{"id", "the drive's id, then /audio/<track number>.wav"},
		Type:  "audio/wav",
		Other: []status{
			{http.StatusConflict, "a job has this drive, or there is no disc"},
			{http.StatusNotFound, "no audio track by that number"},
		},
	}, {
		Method: http.MethodGet, Pattern: "/api/drives/{id}/playlist.m3u", Handler: s.handlePlaylist,
		Summary: "Everything playable on the disc, as a playlist",
		Desc: "For VLC and anything else that takes an m3u. The URLs are absolute, and when " +
			"this server asks for a login they carry an access token, because a player has " +
			"no cookie to send.",
		Param: &param{"id", "the drive's id"},
		Type:  "audio/x-mpegurl",
		Other: []status{{http.StatusNotFound, "there is nothing playable on this disc"}},
	}, {
		Method: http.MethodPost, Pattern: "/api/rip", Handler: s.handleRip,
		Summary: "Start a rip",
		Desc: "Returns immediately with the job; watch it on /api/events. kind is iso for a " +
			"2048-byte-per-sector image, img for the raw 2352-byte sectors with a cue sheet " +
			"beside them, audio for one WAV per track, or files for named paths off the " +
			"disc - one file as itself, several wrapped in the archive format given by " +
			"format.",
		Req: ripRequest{}, Resp: ripResponse{}, Code: http.StatusAccepted,
		Example: ripRequest{Drive: "sr0", Kind: "iso"},
		Other: []status{
			{http.StatusConflict, "a job already has this drive, or there is no disc"},
			{http.StatusBadRequest, "the disc cannot be ripped that way"},
		},
	}, {
		Method: http.MethodPost, Pattern: "/api/scan", Handler: s.handleScan,
		Summary: "Check the condition of a disc",
		Desc: "Reads every sector and counts the bytes the drive's error correction could " +
			"not fix - C2 error pointers, which is the only portable measure of how much " +
			"life a CD has left. A disc reads perfectly right up until it does not; a " +
			"rising error rate is what shows the decline while there is still time to copy " +
			"it. On a DVD, or a drive that will not report C2, the scan falls back to " +
			"unreadable sectors and read speed. The result is kept, so scans of the same " +
			"disc years apart can be compared.",
		Req: scanRequest{}, Resp: ripResponse{}, Code: http.StatusAccepted,
		Example: scanRequest{Drive: "sr0"},
		Other: []status{
			{http.StatusConflict, "a job already has this drive, or there is no disc"},
		},
	}, {
		Method: http.MethodGet, Pattern: "/api/discs", Handler: s.handleDiscs,
		Summary: "Every disc that has been checked, and how it is holding up",
		Desc: "One entry per disc, recognised by a fingerprint taken from its table of " +
			"contents and its volume rather than by its name, with every scan of it in " +
			"order and what changed between the last two. Pass disc=<fingerprint> for one " +
			"disc, which also returns the per-scan damage maps.",
		Resp: discsResponse{},
	}, {
		Method: http.MethodGet, Pattern: "/api/history", Handler: s.handleHistory,
		Summary: "Finished jobs, from the database rather than from memory",
		Desc:    "Survives a restart, which /api/jobs does not. limit defaults to 100.",
		Resp:    historyResponse{},
	}, {
		Method: http.MethodPost, Pattern: "/api/burn", Handler: s.handleBurn,
		Summary: "Write an image to a disc",
		Desc: "Every check that can be made before a disc is spoiled is made first: the " +
			"drive can write this kind of disc, the disc is blank, the image is a whole " +
			"number of sectors and fits. The write itself is done by xorriso or cdrecord. " +
			"Afterwards, unless asked not to, every sector is read back and compared with " +
			"the image and the SHA-256 of the two is checked.",
		Req: burnRequest{}, Resp: ripResponse{}, Code: http.StatusAccepted,
		Example: burnRequest{Drive: "sr0", Image: "debian.iso", SpeedX: 8},
		Other: []status{
			{http.StatusConflict, "the drive, the disc or this server will not allow a burn"},
			{http.StatusNotFound, "no image by that name"},
		},
	}, {
		Method: http.MethodPost, Pattern: "/api/append", Handler: s.handleAppend,
		Summary: "Add files to a disc that is not closed",
		Desc: "Writes a further session. Nothing already on the disc is erased and the " +
			"files that were there stay visible, because the existing filesystem is read " +
			"and added to rather than replaced. Needs xorriso: cdrecord writes whole " +
			"images, which would leave the old files on the disc but invisible. Every " +
			"appended file is then read back off the disc and its SHA-256 compared with " +
			"what was sent.",
		Req: appendRequest{}, Resp: ripResponse{}, Code: http.StatusAccepted,
		Example: appendRequest{Drive: "sr0", Names: []string{"holiday.iso"}, Folder: "/extras"},
		Other: []status{
			{http.StatusConflict, "the disc is blank, closed, full, or this server cannot write"},
			{http.StatusNotFound, "no image by that name"},
			{http.StatusBadRequest, "no files were given, or the folder is not a usable name"},
		},
	}, {
		Method: http.MethodPost, Pattern: "/api/erase", Handler: s.handleErase,
		Summary: "Blank a rewritable disc",
		Desc: "A fast erase clears the table of contents and takes under a minute; a full " +
			"erase rewrites every sector, takes as long as a burn, and is what rescues a " +
			"disc a fast erase has left unwritable.",
		Req: eraseRequest{}, Resp: ripResponse{}, Code: http.StatusAccepted,
		Example: eraseRequest{Drive: "sr0"},
		Other: []status{
			{http.StatusConflict, "there is no disc, or it is not rewritable"},
			{http.StatusForbidden, "this server cannot write to discs"},
		},
	}, {
		Method: http.MethodPost, Pattern: "/api/convert", Handler: s.handleConvert,
		Summary: "Turn a raw .img into an .iso",
		Desc: "Takes the 2048 bytes of user data out of each 2352-byte sector. A raw image " +
			"is the faithful copy; an .iso is the one that can be mounted and burned, and " +
			"this saves going back to the disc for it.",
		Req: convertRequest{}, Resp: ripResponse{}, Code: http.StatusAccepted,
		Example: convertRequest{Name: "disc.img"},
		Other:   []status{{http.StatusBadRequest, "that file is not a raw image"}},
	}, {
		Method: http.MethodGet, Pattern: "/api/jobs", Handler: s.handleJobs,
		Summary: "Every job, newest first",
		Desc:    "Jobs live in memory only: a restart loses the history, not the files.",
		Resp:    jobsResponse{},
	}, {
		Method: http.MethodGet, Pattern: "/api/jobs/{id}", Handler: s.handleJob,
		Summary: "One job",
		Desc:    "The same record the snapshot carries, for a script that would rather poll one job than watch the stream.",
		Param:   &param{"id", "the job id"},
		Resp:    Job{},
		Other:   []status{{http.StatusNotFound, "no job by that id"}},
	}, {
		Method: http.MethodPost, Pattern: "/api/jobs/{id}/cancel", Handler: s.handleJobCancel,
		Summary: "Stop a job",
		Desc: "A cancelled rip removes the half-written file it was producing. A cancelled " +
			"burn stops the burner program, which leaves the disc unusable - there is no " +
			"way to un-write what was already written.",
		Param: &param{"id", "the job id"},
		Resp:  statusMessage{},
		Other: []status{{http.StatusNotFound, "no job by that id"}},
	}, {
		Method: http.MethodGet, Pattern: "/api/state", Handler: s.handleState,
		Summary: "Drives and jobs, once",
		Desc:    "The same snapshot /api/events pushes. Useful to a script; a page should watch the stream.",
		Resp:    snapshot{},
	}, {
		Method: http.MethodGet, Pattern: "/api/events", Handler: s.handleEvents,
		Summary: "Drives and jobs, as they change",
		Desc: "Server-sent events. Every message is a whole snapshot rather than a delta, so " +
			"a browser that reconnects or missed a frame is correct again immediately, and " +
			"nothing is sent at all while nothing changes. A comment line every 25 seconds " +
			"keeps proxies from closing an idle stream.",
		Resp: snapshot{}, Type: "text/event-stream",
	}, {
		Method: http.MethodGet, Pattern: "/api/images", Handler: s.handleLibrary,
		Summary: "Every image in the store",
		Desc: "The store is a flat directory - local, or an SMB share ripperX talks to " +
			"itself. Rips land here and burns are read from here.",
		Resp: libraryResponse{},
	}, {
		Method: http.MethodGet, Pattern: "/api/images/{name}", Handler: s.handleImage,
		Summary: "Download an image, or delete it",
		Desc: "Ranges are honoured, so an interrupted download of a 4 GB image resumes. " +
			"DELETE removes it. A player with no cookie may pass an access token as the " +
			"access_token query parameter.",
		Param: &param{"name", "the file's name, exactly as the listing gives it"},
		Type:  "application/octet-stream",
		Extra: []string{http.MethodDelete},
		Other: []status{{http.StatusNotFound, "no image by that name"}},
	}, {
		Method: http.MethodGet, Pattern: "/api/isos", Handler: s.handleISOs,
		Summary: "The read-only library of images to burn from",
		Desc: "A second location - usually a share of installer images - that ripperX " +
			"burns from and never writes to. Refusing to write to it is structural rather " +
			"than a convention. Empty when none is configured.",
		Resp: libraryResponse{},
	}, {
		Method: http.MethodGet, Pattern: "/api/imageinfo", Handler: s.handleImageInfo,
		Summary: "What an image is, before a disc is spent on it",
		Desc: "Whether it is really an ISO 9660 image, what its volume calls itself, the " +
			"smallest disc it will fit on, and whether a disc written from it will boot - " +
			"and on which firmware. That last is the one question nobody can answer by " +
			"looking at the file, and a disk image meant for a USB stick is the same shape " +
			"as an ISO and fails silently.",
		Param: &param{"name", "the file, as the name query parameter; source picks the library"},
		Resp:  imageInfoResponse{},
		Other: []status{{http.StatusNotFound, "no image by that name"}},
	}, {
		Method: http.MethodPost, Pattern: "/api/upload", Handler: s.handleUpload,
		Summary: "Put an image into the store",
		Desc: "A multipart form with one file in it, which is what a plain file input and " +
			"curl -F both send. The SHA-256 of what arrived comes back, so a truncated " +
			"upload can be told from a complete one before it is burned.",
		Resp: uploadResponse{}, Code: http.StatusCreated, Type: "multipart/form-data",
		Other: []status{
			{http.StatusRequestEntityTooLarge, "larger than this server accepts"},
			{http.StatusBadRequest, "the form had no file in it"},
		},
	}}

	if guard != nil {
		rt = append(rt, route{
			Method: http.MethodPost, Pattern: "/api/login", Handler: guard.handleLogin,
			Summary: "Exchange the configured credentials for tokens",
			Desc: "Sets both tokens as cookies and returns them in the body as well, for a " +
				"script that would rather send Authorization: Bearer. A wrong user and a " +
				"wrong password are reported identically.",
			Req: loginRequest{}, Resp: tokenResponse{},
			Example: loginRequest{User: "ripper", Password: "..."},
			Other: []status{
				{http.StatusBadRequest, "malformed request"},
				{http.StatusUnauthorized, "wrong user name or password"},
			},
		}, route{
			Method: http.MethodPost, Pattern: "/api/refresh", Handler: guard.handleRefresh,
			Summary: "Trade a refresh token for a new pair",
			Desc: "A browser never needs this - a valid refresh cookie renews the access " +
				"token in passing on any request - but a script holding a bearer token does, " +
				"and presents the refresh token in the Authorization header here.",
			Resp:  tokenResponse{},
			Other: []status{{http.StatusUnauthorized, "the refresh token is missing, expired or forged"}},
		}, route{
			Method: http.MethodPost, Pattern: "/api/logout", Handler: guard.handleLogout,
			Summary: "Clear both cookies",
			Desc:    "A bearer token is not revoked by this; it simply expires.",
			Resp:    statusMessage{},
		})
	}

	return append(rt, route{
		Method: http.MethodGet, Pattern: "/api/openapi.json", Handler: s.handleOpenAPI,
		Summary: "This document",
		Desc: "Generated from the route table and from the Go types the handlers decode and " +
			"encode, so it describes what the server does rather than what someone wrote " +
			"down. Whether an endpoint needs a login is shown only when this server was " +
			"started with authentication on.",
		Type: "application/json",
	}, route{
		Method: http.MethodGet, Pattern: "/docs", Handler: s.handleDocs,
		Summary: "The page that renders this document",
		Type:    "text/html",
	})
}
