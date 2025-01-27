package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/anchore/stereoscope/internal/docker"
	"github.com/anchore/stereoscope/pkg/file"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/mitchellh/go-homedir"
	"github.com/spf13/afero"
)

const (
	UnknownSource Source = iota
	DockerTarballSource
	DockerDaemonSource
	OciDirectorySource
	OciTarballSource
	OciRegistrySource
)

const SchemeSeparator = ":"

var sourceStr = [...]string{
	"UnknownSource",
	"DockerTarball",
	"DockerDaemon",
	"OciDirectory",
	"OciTarball",
	"OciRegistry",
}

var AllSources = []Source{
	DockerTarballSource,
	DockerDaemonSource,
	OciDirectorySource,
	OciTarballSource,
	OciRegistrySource,
}

// Source is a concrete a selection of valid concrete image providers.
type Source uint8

// isRegistryReference takes a string and indicates if it conforms to a container image reference.
func isRegistryReference(imageSpec string) bool {
	// note: strict validation requires there to be a default registry (e.g. docker.io) which we cannot assume will be provided
	// we only want to validate the bare minimum number of image specification features, not exhaustive.
	_, err := name.ParseReference(imageSpec, name.WeakValidation)
	return err == nil
}

// ParseSourceScheme attempts to resolve a concrete image source selection from a scheme in a user string.
func ParseSourceScheme(source string) Source {
	source = strings.ToLower(source)
	switch source {
	case "docker-archive":
		return DockerTarballSource
	case "docker":
		return DockerDaemonSource
	case "oci-dir":
		return OciDirectorySource
	case "oci-archive":
		return OciTarballSource
	case "oci-registry", "registry":
		return OciRegistrySource
	}
	return UnknownSource
}

// DetectSource takes a user string and determines the image source (e.g. the docker daemon, a tar file, etc.) returning the string subset representing the image (or nothing if it is unknown).
// note: parsing is done relative to the given string and environmental evidence (i.e. the given filesystem) to determine the actual source.
func DetectSource(userInput string) (Source, string, error) {
	return detectSource(afero.NewOsFs(), userInput)
}

// DetectSource takes a user string and determines the image source (e.g. the docker daemon, a tar file, etc.) returning the string subset representing the image (or nothing if it is unknown).
// note: parsing is done relative to the given string and environmental evidence (i.e. the given filesystem) to determine the actual source.
func detectSource(fs afero.Fs, userInput string) (Source, string, error) {
	candidates := strings.SplitN(userInput, SchemeSeparator, 2)

	var source Source
	var location = userInput
	var sourceHint string
	var err error
	switch len(candidates) {
	case 1:
		// no source hint has been provided, detect one
		source, err = detectSourceFromPath(fs, location)
		if err != nil {
			return UnknownSource, "", err
		}
	case 2:
		// the user may have provided a source hint (or this is a split from a docker image reference, we aren't certain yet)
		sourceHint = candidates[0]
		location = strings.TrimPrefix(userInput, sourceHint+SchemeSeparator)
		source = ParseSourceScheme(sourceHint)
	default:
		source = UnknownSource
	}

	switch source {
	case OciDirectorySource, OciTarballSource, DockerTarballSource:
		// since the scheme was explicitly given, that means that home dir tilde expansion would not have been done by the shell (so we have to)
		location, err = homedir.Expand(location)
		if err != nil {
			return UnknownSource, "", fmt.Errorf("unable to expand potential home dir expression: %w", err)
		}
	case UnknownSource:
		// Ignore any source hint since the source is still unknown. See if this could be a Docker image.
		if imagePullSource := DetermineImagePullSource(userInput); imagePullSource != UnknownSource {
			return imagePullSource, userInput, nil
		}

		// Invalidate any previous processing if the source is still unknown.
		location = ""
	}

	return source, location, nil
}

// DetermineImagePullSource takes an image reference string as input, and
// determines a Source to use to pull the image. If the input doesn't specify an
// image reference (i.e. an image that can be _pulled_), UnknownSource is
// returned. Otherwise, if the Docker daemon is available, DockerDaemonSource is
// returned, and if not, OciRegistrySource is returned.
func DetermineImagePullSource(userInput string) Source {
	if !isRegistryReference(userInput) {
		return UnknownSource
	}

	// verify that the Docker daemon is accessible before assuming we can use it
	dockerClient, err := docker.GetClient()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		pong, err := dockerClient.Ping(ctx)
		if err == nil && pong.APIVersion != "" {
			// the Docker daemon exists and is accessible
			return DockerDaemonSource
		}
	}

	// fallback to using the registry directly
	return OciRegistrySource
}

// DetectSourceFromPath will distinguish between a oci-layout dir, oci-archive, and a docker-archive for a given filesystem.
func DetectSourceFromPath(imgPath string) (Source, error) {
	return detectSourceFromPath(afero.NewOsFs(), imgPath)
}

// detectSourceFromPath will distinguish between a oci-layout dir, oci-archive, and a docker-archive for a given filesystem.
func detectSourceFromPath(fs afero.Fs, imgPath string) (Source, error) {
	imgPath, err := homedir.Expand(imgPath)
	if err != nil {
		return UnknownSource, fmt.Errorf("unable to expand potential home dir expression: %w", err)
	}

	pathStat, err := fs.Stat(imgPath)
	if os.IsNotExist(err) {
		return UnknownSource, nil
	} else if err != nil {
		return UnknownSource, fmt.Errorf("failed to open path=%s: %w", imgPath, err)
	}

	if pathStat.IsDir() {
		//  check for oci-directory
		if _, err := fs.Stat(path.Join(imgPath, "oci-layout")); !os.IsNotExist(err) {
			return OciDirectorySource, nil
		}

		// there are no other directory-based source formats supported
		return UnknownSource, nil
	}

	// assume this is an archive...
	archive, err := fs.Open(imgPath)
	if err != nil {
		return UnknownSource, fmt.Errorf("unable to open archive=%s: %w", imgPath, err)
	}

	for _, pair := range []struct {
		path   string
		source Source
	}{
		{
			"manifest.json",
			DockerTarballSource,
		},
		{
			"oci-layout",
			OciTarballSource,
		},
	} {
		if _, err = archive.Seek(0, io.SeekStart); err != nil {
			return UnknownSource, fmt.Errorf("unable to seek archive=%s: %w", imgPath, err)
		}

		var fileErr *file.ErrFileNotFound
		_, err = file.ReaderFromTar(archive, pair.path)
		if err == nil {
			return pair.source, nil
		} else if !errors.As(err, &fileErr) {
			// short-circuit, there is something wrong with the tar reading process
			return UnknownSource, err
		}
	}

	// there are no other archive-based formats supported
	return UnknownSource, nil
}

// String returns a convenient display string for the source.
func (t Source) String() string {
	return sourceStr[t]
}
