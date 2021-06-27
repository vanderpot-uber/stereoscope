package stereoscope

import (
	"fmt"

	"github.com/anchore/stereoscope/internal/bus"
	"github.com/anchore/stereoscope/internal/log"
	"github.com/anchore/stereoscope/pkg/file"
	"github.com/anchore/stereoscope/pkg/image"
	"github.com/anchore/stereoscope/pkg/image/docker"
	"github.com/anchore/stereoscope/pkg/image/oci"
	"github.com/anchore/stereoscope/pkg/logger"
	"github.com/wagoodman/go-partybus"
)

type CleanupFn func() error

var rootTempDirGenerator = file.NewTempDirGenerator("stereoscope")

// GetImageFromSource returns an image from the explicitly provided source.
func GetImageFromSource(input string, source image.Source, registryOptions *image.RegistryOptions) (*image.Image, CleanupFn, error) {
	var provider image.Provider
	log.Debugf("image: source=%+v location=%+v", source, input)

	tempDirGenerator := rootTempDirGenerator.NewGenerator()

	switch source {
	case image.DockerTarballSource:
		provider = docker.NewProviderFromTarball(input, tempDirGenerator, nil, nil)
	case image.DockerDaemonSource:
		provider = docker.NewProviderFromDaemon(input, tempDirGenerator)
	case image.OciDirectorySource:
		provider = oci.NewProviderFromPath(input, tempDirGenerator)
	case image.OciTarballSource:
		provider = oci.NewProviderFromTarball(input, tempDirGenerator)
	case image.OciRegistrySource:
		provider = oci.NewProviderFromRegistry(input, tempDirGenerator, registryOptions)
	default:
		return nil, tempDirGenerator.Cleanup, fmt.Errorf("unable determine image source")
	}

	img, err := provider.Provide()
	if err != nil {
		return nil, tempDirGenerator.Cleanup, err
	}

	err = img.Read()
	if err != nil {
		return nil, tempDirGenerator.Cleanup, fmt.Errorf("could not read image: %+v", err)
	}

	return img, tempDirGenerator.Cleanup, nil
}

// GetImage parses the user provided image string and provides an image object; note: the source where the image should
// be referenced from is automatically inferred.
func GetImage(userStr string, registryOptions *image.RegistryOptions) (*image.Image, CleanupFn, error) {
	source, input, err := image.DetectSource(userStr)
	if err != nil {
		return nil, func() error { return nil }, err
	}
	return GetImageFromSource(input, source, registryOptions)
}

func SetLogger(logger logger.Logger) {
	log.Log = logger
}

func SetBus(b *partybus.Bus) {
	bus.SetPublisher(b)
}

func Cleanup() {
	if err := rootTempDirGenerator.Cleanup(); err != nil {
		log.Errorf("failed to cleanup: %w", err)
	}
}
