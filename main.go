package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	log "github.com/charmbracelet/log"
	"github.com/github/go-pipe/pipe"
	"github.com/urfave/cli/v2"
)

const poolSize = 8

func cleanupTmpdir(tmpdir string) {
	log.Info("🗑 Cleaning up...")
	rm_args := []string{"-rf", tmpdir}
	rm := exec.Command("rm", rm_args...)
	_, err := rm.CombinedOutput()
	if err != nil {
		log.Fatal(err)
	}
}

var safechars = regexp.MustCompile(`[^a-zA-Z0-9\(\)\-!'" ]+`)

func filesafe(s string) string {
	// replace non-alphanumeric characters with underscores
	s = safechars.ReplaceAllString(s, "_")
	return s

}

func main() {
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "ffmpeg-options",
				Value: "",
				Usage: "options to pass to ffmpeg",
			},
			&cli.StringFlag{
				Name:  "fdkaac-options",
				Value: "-I -p 2 -m 5 -G 0",
				Usage: "options to pass to fdkaac",
			},
			&cli.StringFlag{
				Name:  "log-level",
				Value: "INFO",
				Usage: "log level",
			},
			&cli.StringFlag{
				Name:  "output-dir",
				Value: "",
				Usage: "output directory",
			},
			&cli.StringFlag{
				Name:  "rsync",
				Usage: "rsync destination",
			},
		},
		Action: action,
	}

	if err := app.Run(os.Args); err != nil {
		log.Fatal(err)
	}
}

func set_log_level(level string) {
	switch strings.ToUpper(level) {
	case "DEBUG":
		log.SetLevel(log.DebugLevel)
	case "INFO":
		log.SetLevel(log.InfoLevel)
	case "WARN":
		log.SetLevel(log.WarnLevel)
	case "ERROR":
		log.SetLevel(log.ErrorLevel)
	default:
		log.Fatalf("Unknown log level: %s", level)
	}
}

func action(ctx *cli.Context) error {
	log.SetTimeFormat(time.Kitchen)
	set_log_level(ctx.String("log-level"))

	if ctx.NArg() == 0 {
		log.Fatal("No files specified")
	}

	files := ctx.Args().Slice()

	single_files := []string{}
	for _, filename := range files {
		ext := path.Ext(filename)
		if ext == ".zip" {
			process_zip(ctx, filename)
		} else if ext == ".flac" {
			single_files = append(single_files, filename)
		} else {
			log.Errorf("Unknown file type: %s", filename)
		}
	}

	if len(single_files) > 0 {
		process_single_files(ctx, single_files)
	}

	return nil
}

func output_directory(ctx *cli.Context) string {
	outputdir := ctx.String("output-dir")
	if outputdir == "" {
		// make output directory
		var err error
		outputdir, err = os.MkdirTemp("", "audioconvert")
		if err != nil {
			log.Fatal(err)
		}
	} else {
		os.Mkdir(outputdir, 0755)
	}
	return outputdir
}

func process_single_files(ctx *cli.Context, files []string) {
	outputdir := output_directory(ctx)
	run(ctx, files, outputdir)
}

func process_zip(ctx *cli.Context, filename string) {
	// make a temporary directory for unzipped files
	tmpdir, err := os.MkdirTemp("", "audioconvert")
	if err != nil {
		log.Fatal(err)
	}
	defer cleanupTmpdir(tmpdir)

	outputdir := output_directory(ctx)

	// unzip all files into the temporary directory
	log.Info("🤐 Unzipping", "name", path.Base(filename))
	unzip_args := []string{"-d", tmpdir, filename}
	unzip := exec.Command("unzip", unzip_args...)
	unzip_out, err := unzip.CombinedOutput()
	if err != nil {
		log.Fatal(err)
	}
	log.Debug(string(unzip_out))

	// get all files from the zip
	files, err := filepath.Glob(tmpdir + "/*")
	if err != nil {
		log.Fatal(err)
	}

	// filter out non-audio files
	var audio_files []string
	for _, filename := range files {
		ext := filepath.Ext(filename)
		if ext == ".flac" {
			audio_files = append(audio_files, filename)
		} else if ext == ".jpg" {
			// move artwork to output directory
			log.Info("🎨 Copying artwork", "file", filepath.Base(filename))
			dest := filepath.Join(outputdir, filepath.Base(filename))
			err := os.Rename(filename, dest)
			if err != nil {
				log.Fatal("Failed to move file", "filename", filename, "error", err)
			}
		} else {
			log.Errorf("Unknown file type: %s", filename)
		}
	}
	if len(audio_files) == 0 {
		log.Fatal("No audio files found")
	}

	run(ctx, audio_files, outputdir)
}

func run(ctx *cli.Context, files []string, outputdir string) {
	metadata := get_metadata(files[0])
	log.Info("ℹ️ Metadata", "artist", metadata.Format.Tags.Artist, "album", metadata.Format.Tags.Album)
	log.Info("📀 Transcoding", "count", len(files))
	batch_convert(ctx, files, outputdir)

	var destpath = ctx.String("rsync")
	if destpath != "" {
		// rsync tmpdir over to nova
		dest := fmt.Sprintf("%s/%s/%s", destpath, filesafe(metadata.Format.Tags.Artist), filesafe(metadata.Format.Tags.Album))
		log.Info("📤 Uploading", "destination", dest)
		rsync_args := []string{"-rv", "--mkpath", outputdir + "/", dest + "/"}
		rsync := exec.Command("rsync", rsync_args...)
		rsync_out, err := rsync.CombinedOutput()
		if err != nil {
			log.Error(string(rsync_out))
			log.Fatal(err)
		}
		log.Debug(string(rsync_out))
		// remove outputs
		cleanupTmpdir(outputdir)
	} else {
		log.Info("Output files:", "path", outputdir)
	}
}

func batch_convert(ctx *cli.Context, files []string, tmpdir string) []string {
	work_queue := make(chan string)
	// create a pool of worker goroutines synchoronized with a workgroup
	var wg sync.WaitGroup
	var outputs []string
	wg.Add(poolSize)
	for i := 0; i < poolSize; i++ {
		go func() {
			for filename := range work_queue {
				metadata := get_metadata(filename)
				// convert track to two digits
				track := metadata.Format.Tags.Track
				if len(track) == 1 {
					track = "0" + track
				}
				output := fmt.Sprintf("%s/%s - %s.m4a", tmpdir, track, filesafe(metadata.Format.Tags.Title))
				convert(ctx, filename, output)
				outputs = append(outputs, output)
				// get size of file
				stat, err := os.Stat(output)
				if err != nil {
					log.Fatal(err)
				}
				log.Info("✅ Transcoded", "name", path.Base(output), "size", stat.Size())
			}
			wg.Done()
		}()
	}

	for _, filename := range files {
		work_queue <- filename
	}

	close(work_queue)
	wg.Wait()

	return outputs
}

// metadata struct
type Metadata struct {
	Format struct {
		Filename  string `json:"filename"`
		NbStreams int    `json:"nb_streams"`
		Tags      struct {
			Album  string `json:"album"`
			Artist string `json:"artist"`
			Title  string `json:"title"`
			Track  string `json:"track"`
		}
	}
}

func get_metadata(filename string) Metadata {
	ffprobe_args := []string{"-hide_banner", "-i", filename, "-show_format", "-print_format", "json"}
	ffprobe := exec.Command("ffprobe", ffprobe_args...)
	ffprobe_out, err := ffprobe.Output()
	if err != nil {
		log.Fatal(err)
	}

	// parse into Metadata struct
	var metadata Metadata
	err = json.Unmarshal(ffprobe_out, &metadata)
	if err != nil {
		log.Fatal(err)
	}

	return metadata
}

func convert(ctx *cli.Context, filename string, output string) {
	ffmpeg_args := []string{"-hide_banner", "-i", filename}
	ffmpeg_options := ctx.String("ffmpeg-options")
	if ffmpeg_options != "" {
		ffmpeg_options := strings.Split(ffmpeg_options, " ")
		ffmpeg_args = append(ffmpeg_args, ffmpeg_options...)
	}
	ffmpeg_args = append(ffmpeg_args, "-f", "caf", "-")
	log.Debug("Running ffmpeg", "args", ffmpeg_args)

	fdkaac_args := []string{}
	fdkaac_options := ctx.String("fdkaac-options")
	if fdkaac_options != "" {
		fdkaac_options := strings.Split(fdkaac_options, " ")
		fdkaac_args = append(fdkaac_args, fdkaac_options...)
	}
	fdkaac_args = append(fdkaac_args, "-", "-o", output)
	log.Debug("Running fdkaac", "args", fdkaac_args)

	pipeline := pipe.New(
		pipe.WithEventHandler(func(e *pipe.Event) {
			if exiterr, ok := e.Err.(*exec.ExitError); ok {
				log.Error(string(exiterr.Stderr))
			}
		}),
	)
	dec := pipe.Command("ffmpeg", ffmpeg_args...)
	enc := pipe.Command("fdkaac", fdkaac_args...)
	pipeline.Add(dec, enc)
	err := pipeline.Run(context.Background())
	if err != nil {
		log.Fatal(err)
	}
}
