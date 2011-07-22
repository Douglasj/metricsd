package main

import (
    "flag"
    "exec"
    "net"
    "os"
    "os/signal"
    "path"
    "runtime"
    "time"
    "gorrdpd/config"
    "gorrdpd/logger"
    "gorrdpd/parser"
    "gorrdpd/writers"
    "gorrdpd/stdlib"
    "gorrdpd/types"
    "gorrdpd/web"
)

var (
    log                   logger.Logger     /* Logger instance */
    hostLookupCache       map[string]string /* DNS names cache */
    slices                *types.Slices     /* Slices */
    messagesReceived      int64             /* Messages received */
    totalMessagesReceived int64             /* Total messages received */
    bytesReceived         int64             /* Bytes sent */
    totalBytesReceived    int64             /* Total bytes sent */
)

func main() {
    // Initialize gorrdpd
    initialize()

    // Quit channel. Should be blocking (non-bufferred), so sender
    // will wait till receiver will accept message (and shut down)
    quit := make(chan bool)

    // Active writers
    active_writers := []writers.Writer{
        &writers.Quartiles{},
        &writers.Count{},
    }

    // Start background Go routines
    go listen(quit)
    go stats()
    go dumper(active_writers, quit)
    go web.Start()

    // Handle signals
    for sig := range signal.Incoming {
        var usig = sig.(os.UnixSignal)
        if usig == os.SIGHUP || usig == os.SIGINT || usig == os.SIGTERM {
            log.Warn("Received signal: %s", sig)
            if usig == os.SIGINT || usig == os.SIGTERM {
                log.Warn("Shutting down everything...")
                // We have two background processes, so wait for both
                quit <- true
                quit <- true
            }
            rollupSlices(active_writers, true)
            if usig == os.SIGINT || usig == os.SIGTERM {
                return
            }
        }
    }
}

func initialize() {
    // Initialize options parser
    var slice, write, debug int
    var listen, data, root, cfg string
    var test, batch, lookup bool
    flag.StringVar(&cfg, "config", config.DEFAULT_CONFIG_PATH, "Set the path to config file")
    flag.StringVar(&listen, "listen", config.DEFAULT_LISTEN, "Set the port (+optional address) to listen at")
    flag.StringVar(&data, "data", config.DEFAULT_DATA_DIR, "Set the data directory")
    flag.StringVar(&root, "root", config.DEFAULT_ROOT_DIR, "Set the root directory")
    flag.IntVar(&debug, "debug", int(config.DEFAULT_SEVERITY), "Set the debug level, the lower - the more verbose (0-5)")
    flag.IntVar(&slice, "slice", config.DEFAULT_SLICE_INTERVAL, "Set the slice interval in seconds")
    flag.IntVar(&write, "write", config.DEFAULT_WRITE_INTERVAL, "Set the write interval in seconds")
    flag.BoolVar(&batch, "batch", config.DEFAULT_BATCH_WRITES, "Set the value indicating whether batch RRD updates should be used")
    flag.BoolVar(&lookup, "lookup", config.DEFAULT_LOOKUP_DNS, "Set the value indicating whether reverse DNS lookup should be performed for sources")
    flag.BoolVar(&test, "test", false, "Validate config file and exit")
    flag.Parse()

    // Get root directory
    binaryFile, _ := exec.LookPath(os.Args[0])
    binaryRoot, _ := path.Split(binaryFile)

    // Make config file path absolute
    if cfg[0] != '/' {
        cfg = path.Join(binaryRoot, cfg)
    }
    // Load config from a config file
    config.Global.Load(cfg)
    if test {
        os.Exit(0)
    }

    // Override options with values passed in command line arguments
    // (but only if they have a value different from a default one)
    if listen != config.DEFAULT_LISTEN {
        config.Global.Listen = listen
    }
    if data != config.DEFAULT_DATA_DIR {
        config.Global.DataDir = data
    }
    if data != config.DEFAULT_ROOT_DIR {
        config.Global.RootDir = root
    }
    if debug != int(config.DEFAULT_SEVERITY) {
        config.Global.LogLevel = debug
    }
    if slice != config.DEFAULT_SLICE_INTERVAL {
        config.Global.SliceInterval = slice
    }
    if write != config.DEFAULT_WRITE_INTERVAL {
        config.Global.WriteInterval = write
    }
    if batch != config.DEFAULT_BATCH_WRITES {
        config.Global.BatchWrites = batch
    }
    if lookup != config.DEFAULT_LOOKUP_DNS {
        config.Global.LookupDns = lookup
    }

    // Make data dir path absolute
    if len(config.Global.DataDir) == 0 || config.Global.DataDir[0] != '/' {
        config.Global.DataDir = path.Join(binaryRoot, config.Global.DataDir)
    }

    // Make root dir path absolute
    if len(config.Global.RootDir) == 0 || config.Global.RootDir[0] != '/' {
        config.Global.RootDir = path.Join(binaryRoot, config.Global.RootDir)
    }

    // Create logger
    config.Global.Logger = logger.NewConsoleLogger(logger.Severity(config.Global.LogLevel))
    log = config.Global.Logger
    log.Debug("%s", config.Global)

    // Ensure data directory exists
    if _, err := os.Stat(data); err != nil {
        os.MkdirAll(data, 0755)
    }

    // Resolve listen address
    address, error := net.ResolveUDPAddr("udp", config.Global.Listen)
    if error != nil {
        log.Fatal("Cannot parse \"%s\": %s", config.Global.Listen, error)
        os.Exit(1)
    }
    config.Global.UDPAddress = address

    // Initialize slices structure
    slices = types.NewSlices(config.Global.SliceInterval)

    // Initialize host lookup cache
    if config.Global.LookupDns {
        hostLookupCache = make(map[string]string)
    }

    // Disable memory profiling to prevent panics reporting
    runtime.MemProfileRate = 0
}

/***** Go routines ************************************************************/

func listen(quit chan bool) {
    log.Debug("Starting listener on %s", config.Global.UDPAddress)

    // Listen for requests
    listener, error := net.ListenUDP("udp", config.Global.UDPAddress)
    if error != nil {
        log.Fatal("Cannot listen: %s", error)
        os.Exit(1)
    }
    // Ensure listener will be closed on return
    defer listener.Close()

    // Timeout is 0.1 second
    listener.SetTimeout(100000000)
    listener.SetReadTimeout(100000000)

    message := make([]byte, 256)
    for {
        select {
        case <-quit:
            log.Debug("Shutting down listener...")
            return
        default:
            n, addr, error := listener.ReadFromUDP(message)
            if error != nil {
                if addr != nil {
                    log.Debug("Cannot read UDP from %s: %s\n", addr, error)
                }
                continue
            }
            process(addr, string(message[0:n]))
        }
    }
}

func stats() {
    ticker := time.NewTicker(1000000000)
    defer ticker.Stop()

    for {
        <-ticker.C
        slices.Add(types.NewMessage("all", "gorrdpd$messages_count", int(messagesReceived)))
        slices.Add(types.NewMessage("all", "gorrdpd$traffic_in", int(bytesReceived)))
        slices.Add(types.NewMessage("all", "gorrdpd$memory_used", int(runtime.MemStats.Alloc/1024)))
        slices.Add(types.NewMessage("all", "gorrdpd$memory_system", int(runtime.MemStats.Sys/1024)))

        messagesReceived = 0
        bytesReceived = 0
    }
}

func dumper(active_writers []writers.Writer, quit chan bool) {
    ticker := time.NewTicker(int64(config.Global.WriteInterval) * 1000000000)
    defer ticker.Stop()

    for {
        select {
        case <-quit:
            log.Debug("Shutting down dumper...")
            return
        case <-ticker.C:
            rollupSlices(active_writers, false)
        }
    }
}

/***** Helper functions *******************************************************/

func process(addr *net.UDPAddr, buf string) {
    log.Debug("Processing message from %s: %s", addr, buf)
    bytesReceived += int64(len(buf))
    totalBytesReceived += int64(len(buf))
    parser.Parse(buf, func(message *types.Message, err os.Error) {
        if err == nil {
            if message.Source == "" {
                message.Source = lookupHost(addr)
            }
            slices.Add(message)
            messagesReceived++
            totalMessagesReceived++
        } else {
            log.Debug("Error while parsing a message: %s", err)
        }
    })
}

func lookupHost(addr *net.UDPAddr) (hostname string) {
    ip := addr.IP.String()
    if !config.Global.LookupDns {
        return ip
    }

    // Do we have resolved this address before?
    if _, found := hostLookupCache[ip]; found {
        return hostLookupCache[ip]
    }

    // Try to lookup
    hostname, error := stdlib.GetRemoteHostName(ip)
    if error != nil {
        log.Debug("Error while resolving host name %s: %s", addr, error)
        return ip
    }
    // Cache the lookup result
    hostLookupCache[ip] = hostname

    return
}

func rollupSlices(active_writers []writers.Writer, force bool) {
    log.Debug("Rolling up slices")

    if config.Global.BatchWrites {
        closedSampleSets := slices.ExtractClosedSampleSets(force)
        for _, writer := range active_writers {
            writers.BatchRollup(writer, closedSampleSets)
        }
    } else {
        closedSlices := slices.ExtractClosedSlices(force)
        for _, slice := range closedSlices {
            for _, set := range slice.Sets {
                for _, writer := range active_writers {
                    writers.Rollup(writer, set)
                }
            }
        }
    }
}