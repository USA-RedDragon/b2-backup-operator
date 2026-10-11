package cli

import (
	"bytes"
	"io"

	"k8s.io/cli-runtime/pkg/genericiooptions"
)

const (
	testNS         = "prod"
	testRepoName   = "store"
	testBackupName = "web-files"
	testRepoID     = "abcd1234"
	testServer     = testRepoID + ".repo.borgbase.com"
	testUsage      = "2.1 TiB"
	testQuota      = "4 TiB"
	testTimeZone   = "America/Chicago"
	testSchedule   = "17 2 * * *"
	testCronJobUID = "uid-cronjob"
	reasonReady    = "Ready"
	statusTrue     = "True"
	testClaimName  = "app-data"
	testManualJob  = "web-files-manual-abc"
	testDBTag      = "db-reporting"
	testResticHost = "acme-legacy"
	subjectRepo    = "repository/" + testRepoName
	subjectBackup  = "scheduledbackup/" + testBackupName

	kindRepository      = "Repository"
	kindScheduledBackup = "ScheduledBackup"
)

func testStreams() genericiooptions.IOStreams {
	return genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: io.Discard, ErrOut: io.Discard}
}
