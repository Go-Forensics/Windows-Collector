package windowscollector

import (
	"fmt"
	mft "github.com/AlecRandazzo/GoFor-MFT-Parser"
	log "github.com/sirupsen/logrus"
	syscall "golang.org/x/sys/windows"
	"io"
	"sync"
)

func Collect(exportList ListOfFilesToExport, resultWriter ResultWriter) (err error) {
	log.Debugf("Attempting to acquire the following files %+v", exportList)
	volumesOfInterest, err := identifyVolumesOfInterest(&exportList)
	if err != nil {
		err = fmt.Errorf("identifyVolumesOfInterest() returned an error: %w", err)
		return
	}

	searchTerms, err := setupSearchTerms(exportList)
	if err != nil {
		err = fmt.Errorf("setupSearchTerms() returned the following error: %w", err)
		return
	}

	for _, volumeLetter := range volumesOfInterest {
		var volumeHandler VolumeHandler
		volumeHandler, err = GetVolumeHandler(volumeLetter)
		if err != nil {
			err = fmt.Errorf("GetVolumeHandler() failed to get a handle to the volume %s: %w", volumeLetter, err)
			return
		}

		err = getFiles(volumeHandler, resultWriter, searchTerms)
		if err != nil {
			err = fmt.Errorf("getFiles() failed to get files: %w", err)
			return
		}
	}
	return
}

func getFiles(volumeHandler VolumeHandler, resultWriter ResultWriter, listOfSearchKeywords listOfSearchTerms) (err error) {
	// Init a few things
	fileReaders := make(chan fileReader, 100)
	waitForInitialization := sync.WaitGroup{}
	waitForFileCopying := sync.WaitGroup{}

	waitForInitialization.Add(1)
	waitForFileCopying.Add(1)
	go func() {
		err = resultWriter.ResultWriter(&fileReaders, &waitForInitialization, &waitForFileCopying)
	}()
	waitForInitialization.Wait()
	if err != nil {
		err = fmt.Errorf("failed to setup resultWriter: %v", err)
		return
	}
	log.Debug("Successfully initialized the ResultWriter goroutine.")

	// parse the mft's mft record to get its dataruns
	mftRecord0, err := parseMFTRecord0(volumeHandler)
	if err != nil {
		err = fmt.Errorf("parseMFTRecord0() failed to parse mft record 0 from the volume %s: %w", volumeHandler.VolumeLetter, err)
		return
	}
	log.Debugf("Parsed the MFT's MFT record and got the following: %+v", mftRecord0)

	// Go back to the beginning of the mft record
	_, _ = syscall.Seek(volumeHandler.Handle, volumeHandler.Vbr.MftByteOffset, 0)
	log.Debugf("Seeked back to the beginning offset to the MFT at offset %d", volumeHandler.Vbr.MftByteOffset)

	// Open a raw reader on the MFT
	foundFile := foundFile{
		dataAttribute: mftRecord0.DataAttribute,
		fullPath:      "$mft",
	}
	mftReader := rawFileReader(volumeHandler, foundFile)
	log.Debug("Obtained a raw io.Reader to the MFT's dataruns.")

	// Do we need to stream a copy of the mft while we read it?
	areWeCopyingTheMFT := false
	directoryTree := mft.DirectoryTree{}
	possibleMatches := possibleMatches{}

	for index, value := range listOfSearchKeywords {
		if value.fileNameString == "$mft" {
			areWeCopyingTheMFT = true

			// delete this from our search list
			listOfSearchKeywords[index] = listOfSearchKeywords[len(listOfSearchKeywords)-1]
			listOfSearchKeywords = listOfSearchKeywords[:len(listOfSearchKeywords)-1]

			break
		}
	}

	if areWeCopyingTheMFT == true {
		log.Debug("We are configured to grab a copy of the MFT, so we'll set up a io.TeeReader with an io.Pipe so we can copy the mft as we read it. We do this so we only have to read the MFT's data runs once and only once.")
		pipeReader, pipeWriter := io.Pipe()
		teeReader := io.TeeReader(mftReader, pipeWriter)
		fileReader := fileReader{
			fullPath: fmt.Sprintf("%s__$mft", volumeHandler.VolumeLetter),
			reader:   pipeReader,
		}
		fileReaders <- fileReader
		volumeHandler.mftReader = teeReader
		possibleMatches, directoryTree, err = findPossibleMatches(volumeHandler, listOfSearchKeywords)
		if err != nil {
			err = fmt.Errorf("findPossibleMatches() failed: %w", err)
			return
		}
		err = pipeWriter.Close()
		if err != nil {
			err = fmt.Errorf("failed to close writer pipe: %w", err)
			return
		}
	} else {
		volumeHandler.mftReader = mftReader
		possibleMatches, directoryTree, err = findPossibleMatches(volumeHandler, listOfSearchKeywords)
		if err != nil {
			err = fmt.Errorf("findPossibleMatches() failed: %w", err)
			return
		}
	}

	foundFiles, err := confirmFoundFiles(listOfSearchKeywords, possibleMatches, directoryTree)
	if err != nil {
		err = fmt.Errorf("confirmFoundFiles() failed with error: %w", err)
		return
	}

	for _, file := range foundFiles {
		// try to get an io.reader via api first
		log.Debugf("Trying to get an io.Reader from the file %s via API.", file.fullPath)
		reader, err := apiFileReader(file)
		if err != nil {
			log.Debugf("Failed to get an io.Reader via API method, trying via raw method against the file's '%s' dataruns: %+v", file.fullPath, file.dataAttribute)
			// failed to get an API handle, trying to get an io.reader via raw method
			reader = rawFileReader(volumeHandler, file)
		}
		fileReader := fileReader{
			fullPath: file.fullPath,
			reader:   reader,
		}
		log.Debugf("Passing a fileReader for %s to our ResultWriter", fileReader.fullPath)
		fileReaders <- fileReader
	}
	close(fileReaders)
	err = nil
	waitForFileCopying.Wait()
	return
}
