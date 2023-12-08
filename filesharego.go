package filesharego

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	icore "github.com/ipfs/boxo/coreiface"
	"github.com/ipfs/boxo/coreiface/options"
	"github.com/ipfs/boxo/path"
	"github.com/ipfs/go-cid"
	files "github.com/ipfs/go-ipfs-files"
	"github.com/ipfs/kubo/config"
	"github.com/ipfs/kubo/core"
	"github.com/ipfs/kubo/core/coreapi"
	"github.com/ipfs/kubo/core/node/libp2p"
	"github.com/ipfs/kubo/plugin/loader"
	"github.com/ipfs/kubo/repo/fsrepo"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/schollz/progressbar/v3"
)

var flagExp = flag.Bool("experimental", false, "enable experimental features")

func SetupPlugins(externalPluginsPath string) error {
	// Load any external plugins if available on externalPluginsPath
	plugins, err := loader.NewPluginLoader(filepath.Join(externalPluginsPath, "plugins"))
	if err != nil {
		return fmt.Errorf("error loading plugins: %s", err)
	}

	// Load preloaded and external plugins
	if err := plugins.Initialize(); err != nil {
		return fmt.Errorf("error initializing plugins: %s", err)
	}

	if err := plugins.Inject(); err != nil {
		return fmt.Errorf("error initializing plugins: %s", err)
	}

	return nil
}

func CreateTempRepo() (string, error) {
	repoPath, err := os.MkdirTemp("", "ipfs-shell")
	if err != nil {
		return "", fmt.Errorf("failed to get temp dir: %s", err)
	}

	// Create a config with default options and a 2048 bit key
	cfg, err := config.Init(io.Discard, 2048)
	if err != nil {
		return "", err
	}

	// When creating the repository, you can define custom settings on the repository, such as enabling experimental
	// features (See experimental-features.md) or customizing the gateway endpoint.
	// To do such things, you should modify the variable `cfg`. For example:
	if *flagExp {
		// https://github.com/ipfs/kubo/blob/master/docs/experimental-features.md#ipfs-filestore
		cfg.Experimental.FilestoreEnabled = true
		// https://github.com/ipfs/kubo/blob/master/docs/experimental-features.md#ipfs-urlstore
		cfg.Experimental.UrlstoreEnabled = true
		// https://github.com/ipfs/kubo/blob/master/docs/experimental-features.md#ipfs-p2p
		cfg.Experimental.Libp2pStreamMounting = true
		// https://github.com/ipfs/kubo/blob/master/docs/experimental-features.md#p2p-http-proxy
		cfg.Experimental.P2pHttpProxy = true
		// See also: https://github.com/ipfs/kubo/blob/master/docs/config.md
		// And: https://github.com/ipfs/kubo/blob/master/docs/experimental-features.md
	}

	// Create the repo with the config
	err = fsrepo.Init(repoPath, cfg)
	if err != nil {
		return "", fmt.Errorf("failed to init ephemeral node: %s", err)
	}

	return repoPath, nil
}

/// ------ Spawning the node

// Creates an IPFS node and returns its coreAPI.
func CreateNode(ctx context.Context, repoPath string) (*core.IpfsNode, error) {
	// Open the repo
	repo, err := fsrepo.Open(repoPath)
	if err != nil {
		return nil, err
	}

	// Construct the node

	nodeOptions := &core.BuildCfg{
		Online:  true,
		Routing: libp2p.DHTOption, // This option sets the node to be a full DHT node (both fetching and storing DHT Records)
		// Routing: libp2p.DHTClientOption, // This option sets the node to be a client DHT node (only fetching records)
		Repo: repo,
	}

	return core.NewNode(ctx, nodeOptions)
}

var loadPluginsOnce sync.Once

// Spawns a node to be used just for this run (i.e. creates a tmp repo).
func SpawnEphemeral(ctx context.Context) (icore.CoreAPI, *core.IpfsNode, error) {
	var onceErr error
	loadPluginsOnce.Do(func() {
		onceErr = SetupPlugins("")
	})
	if onceErr != nil {
		return nil, nil, onceErr
	}

	// Create a Temporary Repo
	repoPath, err := CreateTempRepo()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create temp repo: %s", err)
	}

	node, err := CreateNode(ctx, repoPath)
	if err != nil {
		return nil, nil, err
	}

	api, err := coreapi.NewCoreAPI(node)

	return api, node, err
}

// for the future file sharing
func ConnectToPeers(ctx context.Context, ipfs icore.CoreAPI, peers []string) error {
	var wg sync.WaitGroup
	peerInfos := make(map[peer.ID]*peer.AddrInfo, len(peers))
	for _, addrStr := range peers {
		addr, err := ma.NewMultiaddr(addrStr)
		if err != nil {
			return err
		}
		pii, err := peer.AddrInfoFromP2pAddr(addr)
		if err != nil {
			return err
		}
		pi, ok := peerInfos[pii.ID]
		if !ok {
			pi = &peer.AddrInfo{ID: pii.ID}
			peerInfos[pi.ID] = pi
		}
		pi.Addrs = append(pi.Addrs, pii.Addrs...)
	}

	wg.Add(len(peerInfos))
	for _, peerInfo := range peerInfos {
		go func(peerInfo *peer.AddrInfo) {
			defer wg.Done()
			err := ipfs.Swarm().Connect(ctx, *peerInfo)
			if err != nil {
				log.Printf("failed to connect to %s: %s", peerInfo.ID, err)
			}
		}(peerInfo)
	}
	wg.Wait()
	return nil
}

func GetUnixfsNode(path string) (files.Node, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	f, err := files.NewSerialFile(path, false, st)
	if err != nil {
		return nil, err
	}

	return f, nil
}

func ForeverSpin() {
	bar := progressbar.Default(-1)
	// bar := progressbar.DefaultBytes(
	// 	-1,
	// 	"uploading",
	// )
	for {
		// for i := 0; i < 100; i++ {
		// 	bar.Add(1)
		// 	time.Sleep(40 * time.Millisecond)
		// }
		bar.Add(1)
		time.Sleep(100 * time.Millisecond)
	}
}

func DownloadFromCid(ctx context.Context, ipfsA icore.CoreAPI, cidStr string) {
	// in case of /ipfs/exampleCid we strip string and work only on exampleCid
	cidStr = cidStr[strings.LastIndex(cidStr, "/")+1:]
	cidFromString, err := cid.Parse(cidStr)
	if err != nil {
		panic(fmt.Errorf("could not get CID from flag cid: %s", err))
	}

	fmt.Printf("Fetching a file from the network with CID %s\n", cidStr)
	testCID := path.FromCid(cidFromString)

	rootNode, err := ipfsA.Unixfs().Get(ctx, testCID)
	if err != nil {
		panic(fmt.Errorf("could not get file with CID: %s", err))
	}

	// for the future simplicity to download single files in the same directory. Opened ticked on ipfs here: https://github.com/ipfs/boxo/issues/520
	// c, err := ipfsA.Unixfs().Ls(ctx, testCID)
	// if err != nil {
	// 	panic(fmt.Errorf("could not find Ls info from Cid: %s", err))
	// }
	// fileCounter := 0
	// for de := range c {
	// 	fileCounter += 1
	// 	fmt.Printf("%d file name: %v\n", fileCounter, de.Name)
	// }

	outputPath := "./Download/" + cidStr

	err = os.MkdirAll("Download", 0o777)
	if err != nil {
		panic(fmt.Errorf("could not create Download directory: %s", err))
	}

	err = files.WriteTo(rootNode, outputPath)
	if err != nil {
		panic(fmt.Errorf("could not write out the fetched CID: %s\noutputPath: %s", err, outputPath))
	}

	fmt.Printf("Wrote the files to %s\n", outputPath)
}

func UploadFromPath(ctx context.Context, ipfsA icore.CoreAPI, filePathStr string) {
	someFile, err := GetUnixfsNode(filePathStr)
	if err != nil {
		panic(fmt.Errorf("could not get File: %s", err))
	}

	fileInfo, err := os.Stat(filePathStr)
	if err != nil {
		panic(fmt.Errorf("could not get File stat info: %s", err))
	}

	// wrap file into directory with filename so ipfs shows file name later
	if !fileInfo.IsDir() {
		someFile = files.NewSliceDirectory([]files.DirEntry{
			files.FileEntry(filepath.Base(filePathStr), someFile),
		})
	}

	cidFile, err := ipfsA.Unixfs().Add(ctx, someFile, options.Unixfs.Pin(true))
	if err != nil {
		panic(fmt.Errorf("could not add File: %s", err))
	}

	fmt.Printf("Added file to IPFS. Now share this CID with your friend:\n%s\nor open new cmd window and download with command:\nfilesharego -c %s\n", cidFile.String(), cidFile.String())

	// for the future simplicity to download single files in the same directory. Opened ticked on ipfs here: https://github.com/ipfs/boxo/issues/520
	// c, err := ipfsA.Unixfs().Ls(ctx, cidFile)
	// if err != nil {
	// 	panic(fmt.Errorf("could not find Ls info from Cid: %s", err))
	// }
	// fileCounter := 0
	// for de := range c {
	// 	fileCounter += 1
	// 	fmt.Printf("%d file name: %v\n", fileCounter, de.Name)
	// }

	fileSize, err := someFile.Size()
	if err != nil {
		panic(fmt.Errorf("could not get File: %s", err))
	}

	fmt.Printf("Seeding size: %s\n", humanize.Bytes(uint64(fileSize)))

	go ForeverSpin()

	quitChannel := make(chan os.Signal, 1)
	signal.Notify(quitChannel, syscall.SIGINT, syscall.SIGTERM)
	<-quitChannel

	fmt.Println("\nAdios!")
}
