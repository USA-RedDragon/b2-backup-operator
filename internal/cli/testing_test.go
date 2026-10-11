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
	testKeyID      = "0055aa00000000000000000001"
	testBucket     = "acme-backups"
	testPath       = "restic/prod"
	testEndpoint   = "https://s3.us-east-005.backblazeb2.com"
	testRepoURL    = "s3:" + testEndpoint + "/" + testBucket + "/" + testPath
	testUsage      = "2.1TiB"
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
