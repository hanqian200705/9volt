package main

import (
	"fmt"
	"strings"
	"sync"

	"github.com/InVisionApp/rye"
	log "github.com/Sirupsen/logrus"
	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/9corp/9volt/alerter"
	"github.com/9corp/9volt/api"
	"github.com/9corp/9volt/base"
	"github.com/9corp/9volt/cfgutil"
	"github.com/9corp/9volt/cluster"
	"github.com/9corp/9volt/config"
	"github.com/9corp/9volt/dal"
	"github.com/9corp/9volt/director"
	"github.com/9corp/9volt/event"
	"github.com/9corp/9volt/manager"
	"github.com/9corp/9volt/overwatch"
	"github.com/9corp/9volt/state"
	"github.com/9corp/9volt/util"
)

const (
	DEFAULT_VERSION = "N/A"
	DEFAULT_SEMVER  = "N/A"
)

var (
	server        = kingpin.Command("server", "9volt server")
	listenAddress = server.Flag("listen", "Address for 9volt's API to listen on").Short('l').Default("0.0.0.0:8080").Envar("NINEV_LISTEN_ADDRESS").String()
	tags          = server.Flag("tags", "Specify one or more member tags this instance has; see MONITOR_CONFIGS.md for details").Short('t').Envar("NINEV_MEMBER_TAGS").String()
	accessTokens  = server.Flag("access-tokens", "Specify required access tokens in the header for API requests").Short('a').PlaceHolder("token1,token2").Envar("NINEV_ACCESS_TOKENS").String()

	cfg         = kingpin.Command("cfg", "9volt configuration utility")
	dirArg      = cfg.Arg("dir", "Directory to search for 9volt YAML files").Required().String()
	replaceFlag = cfg.Flag("replace", "Do NOT verify if parsed config already exists in etcd (ie. replace everything)").Short('r').Bool()
	nosyncFlag  = cfg.Flag("nosync", "Do NOT remove any entries in etcd that do not have a corresponding local config").Short('n').Bool()
	dryrunFlag  = cfg.Flag("dryrun", "Do NOT push any changes, just show me what you'd do").Bool()

	etcdPrefix   = kingpin.Flag("etcd-prefix", "Prefix that 9volt's configuration is stored under in etcd").Short('p').Default("9volt").Envar("NINEV_ETCD_PREFIX").String()
	etcdMembers  = kingpin.Flag("etcd-members", "List of etcd cluster members").Short('e').Default("http://localhost:2379").Envar("NINEV_ETCD_MEMBERS").String()
	etcdUserPass = kingpin.Flag("etcd-userpass", "Username/Password for authenticated etcd user").Short('U').PlaceHolder("\"username:password\"").Envar("NINEV_ETCD_USERPASS").String()
	debugUI      = kingpin.Flag("debug-ui", "Debug the user interface locally").Short('u').Bool()
	debug        = kingpin.Flag("debug", "Enable debug mode").Short('d').Envar("NINEV_DEBUG").Bool()

	version string
	semver  string
	command string
)

func init() {
	log.SetLevel(log.InfoLevel)

	// Friendlier versions
	if version == "" {
		version = DEFAULT_VERSION
	}

	if semver == "" {
		semver = DEFAULT_SEMVER
	}

	// Parse CLI stuff
	kingpin.Version(semver + " - " + version)
	kingpin.CommandLine.HelpFlag.Short('h')
	kingpin.CommandLine.VersionFlag.Short('v')
	command = kingpin.Parse()

	if *debug {
		log.SetLevel(log.DebugLevel)
	}
}

func runServer() {
	var wg sync.WaitGroup
	wg.Add(1)

	memberID := util.GetMemberID(*listenAddress)

	// kingpin splits on newline (?); split our tags on ',' instead
	memberTags := make([]string, 0)

	if *tags != "" {
		memberTags = util.SplitTags(*tags)
	}

	etcdMemberList := util.SplitTags(*etcdMembers)

	log.WithFields(log.Fields{
		"memberID": memberID,
		"tags":     memberTags,
	}).Info("Starting 9volt server")

	// Create an initial dal client
	dalClient, err := dal.New(*etcdPrefix, etcdMemberList, *etcdUserPass, false, false, false)
	if err != nil {
		log.WithField("err", err).Fatal("Unable to start initial etcd client")
	}

	// Create and start event queue
	eventQueue := event.NewQueue(memberID, dalClient)
	eqClient := eventQueue.NewClient()

	// Load our configuration
	cfg := config.New(memberID, *listenAddress, *etcdPrefix, *etcdUserPass, etcdMemberList,
		memberTags, dalClient, eqClient, version, semver)

	if err := cfg.Load(); err != nil {

		log.WithField("err", err).Fatal("Unable to load configuration from etcd")
	}

	// Perform etcd layout validation
	if errorList := cfg.ValidateDirs(); len(errorList) != 0 {
		log.WithField("errorList", strings.Join(errorList, "; ")).Fatal("Unable to complete etcd layout validation")
	}

	// Create necessary channels
	clusterStateChannel := make(chan bool)
	distributeChannel := make(chan bool)
	messageChannel := make(chan *alerter.Message)
	monitorStateChannel := make(chan *state.Message)
	overwatchChannel := make(chan *overwatch.Message)

	// Instantiate all of the components
	cluster, err := cluster.New(cfg, clusterStateChannel, distributeChannel, overwatchChannel)
	if err != nil {
		log.WithField("err", err).Fatal("Unable to instantiate cluster engine")
	}

	director, err := director.New(cfg, clusterStateChannel, distributeChannel, overwatchChannel)
	if err != nil {
		log.WithField("err", err).Fatal("Unable to instantiate director")
	}

	manager, err := manager.New(cfg, messageChannel, monitorStateChannel, overwatchChannel)
	if err != nil {
		log.WithField("err", err).Fatal("Unable to instantiate manager")
	}

	alerter := alerter.New(cfg, messageChannel)
	state := state.New(cfg, monitorStateChannel)

	// Start all of the components (start order matters!)
	components := []base.IComponent{cluster, director, manager, alerter, state, eventQueue}

	watcher := overwatch.New(cfg, overwatchChannel, components)
	if err := watcher.Start(); err != nil {
		log.WithField("err", err).Fatal("Unable to start overwatch component")
	}

	for _, cmp := range components {
		log.WithField("component", cmp.Identify()).Info("Starting component")

		if err := cmp.Start(); err != nil {
			log.WithFields(log.Fields{
				"component": cmp.Identify(),
				"err":       err,
			}).Fatal("Unable to start component")
		}
	}

	// create a new middleware handler
	mwHandler := rye.NewMWHandler(rye.Config{})

	// determines whether or not to use statik or debug interactively
	debugUserInterface := false
	if *debugUI {
		debugUserInterface = true
	}

	// start api server
	apiServer := api.New(cfg, mwHandler, debugUserInterface, util.SplitTags(*accessTokens))
	go apiServer.Run()

	log.WithFields(log.Fields{
		"listenAddress": *listenAddress,
		"memberID":      memberID,
		"tags":          strings.Join(memberTags, ", "),
	}).Info("9volt has started!")

	wg.Wait()
}

func runCfgUtil() {
	etcdMemberList := util.SplitTags(*etcdMembers)

	etcdClient, err := dal.New(*etcdPrefix, etcdMemberList, *etcdUserPass, *replaceFlag, *dryrunFlag, *nosyncFlag)
	if err != nil {
		log.Fatalf("Unable to create initial etcd client: %v", err.Error())
	}

	// verify if given dirArg is actually a dir
	cfg, err := cfgutil.New(*dirArg)
	if err != nil {
		log.Fatal(err.Error())
	}

	log.Infof("Fetching all 9volt configuration files in '%v'", *dirArg)

	yamlFiles, err := cfg.Fetch()
	if err != nil {
		log.Fatalf("Unable to fetch config files from dir '%v': %v", *dirArg, err.Error())
	}

	log.Info("Parsing 9volt config files")

	configs, err := cfg.Parse(yamlFiles)
	if err != nil {
		log.Fatalf("Unable to complete config file parsing: %v", err.Error())
	}

	log.Infof("Found %v alerter configs and %v monitor configs", len(configs.AlerterConfigs), len(configs.MonitorConfigs))
	log.Infof("Pushing 9volt configs to etcd hosts: %v", *etcdMembers)

	// push to etcd
	stats, errorList := etcdClient.PushFullConfigs(configs)
	if len(errorList) != 0 {
		log.Errorf("Encountered %v errors: %v", len(errorList), errorList)
	}

	pushedMessage := fmt.Sprintf("pushed %v monitor config(s) and %v alerter config(s)", stats.MonitorAdded, stats.AlerterAdded)
	skippedMessage := fmt.Sprintf("skipped replacing %v monitor config(s) and %v alerter config(s)", stats.MonitorSkipped, stats.AlerterSkipped)
	removedMessage := fmt.Sprintf("removed %v monitor config(s) and %v alerter config(s)", stats.MonitorRemoved, stats.AlerterRemoved)

	if *dryrunFlag {
		pushedMessage = "DRYRUN: Would have " + pushedMessage
		skippedMessage = "DRYRUN: Would have " + skippedMessage
		removedMessage = "DRYRUN: Would have " + removedMessage
	} else {
		pushedMessage = ":party: Successfully " + pushedMessage
		skippedMessage = "Successfully " + skippedMessage
		removedMessage = "Successfully " + removedMessage
	}

	log.Info(pushedMessage)

	if !*replaceFlag {
		log.Info(skippedMessage)
	}

	if !*nosyncFlag {
		log.Info(removedMessage)
	}
}

func main() {
	switch command {
	case "server":
		runServer()
	case "cfg":
		runCfgUtil()
	}
}
