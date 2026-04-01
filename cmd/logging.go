package cmd

import (
	"fmt"
	"os"
	"time"

	"charm.land/log/v2"
	"github.com/charmbracelet/colorprofile"
)

// setupLogging configures the log package based on the DEBUG environment variable.
// When DEBUG=true, logs are written to debug.log at debug level.
// Otherwise, only fatal messages are written to stderr.
// Returns a cleanup function that should be deferred by the caller.
func setupLogging() func() {
	if os.Getenv("DEBUG") == "true" {
		logFile, err := os.OpenFile("debug.log",
			os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o666)
		if err != nil {
			fmt.Println("Error opening debug.log:", err)
			os.Exit(1)
		}

		log.SetOutput(logFile)
		log.SetTimeFormat(time.Kitchen)
		log.SetReportCaller(true)
		log.SetLevel(log.DebugLevel)
		log.SetColorProfile(colorprofile.TrueColor)

		wd, err := os.Getwd()
		if err != nil {
			fmt.Println("Error getting current working dir", err)
			os.Exit(1)
		}
		log.Debug("🚀 Starting diffnav", "logFile",
			wd+string(os.PathSeparator)+logFile.Name())

		return func() {
			if err := logFile.Close(); err != nil {
				log.Fatal("failed closing log file", "err", err)
			}
		}
	}

	log.SetOutput(os.Stderr)
	log.SetLevel(log.FatalLevel)
	return func() {}
}
