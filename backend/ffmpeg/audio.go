package ffmpeg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gtsteffaniak/filebrowser/backend/common/settings"
	"github.com/gtsteffaniak/filebrowser/backend/common/utils"
	"github.com/gtsteffaniak/go-logger/logger"
)

// audioExtractLocks serializes extraction per cache file so concurrent requests
// for the same track don't run duplicate ffmpeg processes.
var audioExtractLocks sync.Map

// DetectAudioTracks detects embedded audio streams using ffprobe.
// Returns nil if ffprobe is not available or fails. Results are cached by path + modtime.
func DetectAudioTracks(videoPath string, modtime time.Time) []utils.AudioTrack {
	if settings.Env.FFprobePath == "" {
		return nil
	}
	key := "audio_tracks:" + videoPath + ":" + modtime.Format(time.RFC3339)
	if cached, ok := AudioTrackCache.Get(key); ok {
		return cached
	}
	tracks := detectAudioTracks(videoPath)
	AudioTrackCache.Set(key, tracks)
	return tracks
}

func detectAudioTracks(realPath string) []utils.AudioTrack {
	cmd := exec.Command(settings.Env.FFprobePath,
		"-v", "quiet",
		"-print_format", "json",
		"-show_streams",
		"-select_streams", "a",
		realPath)

	output, err := cmd.Output()
	if err != nil {
		logger.Debug("ffprobe failed for file: " + realPath + ", error: " + err.Error())
		return nil
	}

	var probeOutput FFProbeOutput
	if err := json.Unmarshal(output, &probeOutput); err != nil {
		logger.Debug("failed to parse ffprobe output for file: " + realPath)
		return nil
	}

	var tracks []utils.AudioTrack
	for _, stream := range probeOutput.Streams {
		if stream.CodecType != "audio" {
			continue
		}
		track := utils.AudioTrack{
			Index:    stream.Index,
			Codec:    stream.CodecName,
			Channels: stream.Channels,
			Default:  stream.Disposition["default"] == 1,
		}
		if stream.Tags != nil {
			if lang, ok := stream.Tags["language"]; ok {
				track.Language = lang
			}
			if title, ok := stream.Tags["title"]; ok {
				track.Title = title
			}
		}
		if track.Title != "" {
			track.Name = track.Title
		} else if track.Language != "" {
			track.Name = track.Language
		} else {
			track.Name = "Track " + strconv.Itoa(len(tracks)+1)
		}
		tracks = append(tracks, track)
	}
	return tracks
}

// audioExtractionFormat maps a source codec to the ffmpeg output arguments,
// container format, file extension, and mime type used when extracting a track.
// Browser-safe codecs are stream-copied; everything else (ac3, eac3, dts, truehd,
// pcm, ...) is transcoded to AAC.
func audioExtractionFormat(codec string) (codecArgs []string, format, ext, mime string) {
	switch codec {
	case "aac":
		return []string{"-c:a", "copy", "-movflags", "+faststart"}, "mp4", ".m4a", "audio/mp4"
	case "mp3":
		return []string{"-c:a", "copy"}, "mp3", ".mp3", "audio/mpeg"
	case "opus":
		return []string{"-c:a", "copy"}, "webm", ".webm", "audio/webm"
	case "vorbis":
		return []string{"-c:a", "copy"}, "ogg", ".ogg", "audio/ogg"
	case "flac":
		return []string{"-c:a", "copy"}, "flac", ".flac", "audio/flac"
	default:
		return []string{"-c:a", "aac", "-b:a", "256k", "-movflags", "+faststart"}, "mp4", ".m4a", "audio/mp4"
	}
}

// ExtractAudioTrack extracts a single embedded audio stream to a cached file and
// returns its path and mime type. The cache key includes the file modtime, so
// changed files re-extract while previous results are reused across requests.
func ExtractAudioTrack(ctx context.Context, videoPath string, streamIndex int, codec string, modtime time.Time) (string, string, error) {
	if settings.Env.FFmpegPath == "" {
		return "", "", fmt.Errorf("ffmpeg is not available")
	}
	codecArgs, format, ext, mime := audioExtractionFormat(codec)

	cacheDir := settings.Config.Server.CacheDir
	if cacheDir == "" {
		cacheDir = os.TempDir()
	}
	cacheKey := utils.HashSHA256(fmt.Sprintf("%s:%d:%d", videoPath, modtime.Unix(), streamIndex))
	cachePath := filepath.Join(cacheDir, "audio_tracks", cacheKey+ext)

	lock, _ := audioExtractLocks.LoadOrStore(cachePath, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	if _, err := os.Stat(cachePath); err == nil {
		return cachePath, mime, nil
	}
	if err := os.MkdirAll(filepath.Dir(cachePath), 0700); err != nil {
		return "", "", fmt.Errorf("failed to create audio track cache directory: %v", err)
	}

	tmpPath := cachePath + ".tmp" + ext
	args := []string{
		"-y",
		"-i", videoPath,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-map_chapters", "-1",
		"-vn", "-sn", "-dn",
	}
	args = append(args, codecArgs...)
	args = append(args, "-f", format, tmpPath)

	cmd := exec.CommandContext(ctx, settings.Env.FFmpegPath, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(tmpPath)
		logger.Debugf("audio track extraction failed for %s stream %d: %v: %s", videoPath, streamIndex, err, string(output))
		return "", "", fmt.Errorf("audio track extraction failed: %v", err)
	}
	if err := os.Rename(tmpPath, cachePath); err != nil {
		os.Remove(tmpPath)
		return "", "", fmt.Errorf("failed to finalize extracted audio track: %v", err)
	}
	return cachePath, mime, nil
}
