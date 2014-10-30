/*
Copyright 2014 Google Inc. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package manager

import (
	"cups-connector/cups"
	"cups-connector/gcp"
	"cups-connector/lib"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang/glog"
)

// Manages all interactions between CUPS and Google Cloud Print.
type PrinterManager struct {
	cups               *cups.CUPS
	gcp                *gcp.GoogleCloudPrint
	gcpPrintersByGCPID map[string]lib.Printer
	gcpJobPollQuit     chan bool
	printerPollQuit    chan bool
	downloadSemaphore  *lib.Semaphore
	jobStatsSemaphore  *lib.Semaphore
	jobsDone           uint
	jobsError          uint
	cupsQueueSize      uint
	jobPollInterval    time.Duration
	jobFullUsername    bool
}

func NewPrinterManager(cups *cups.CUPS, gcp *gcp.GoogleCloudPrint, printerPollInterval, jobPollInterval, gcpMaxConcurrentDownload, cupsQueueSize uint, jobFullUsername bool) (*PrinterManager, error) {
	gcpPrinters, err := gcp.List()
	if err != nil {
		return nil, err
	}
	gcpPrintersByGCPID := make(map[string]lib.Printer, len(gcpPrinters))
	for _, p := range gcpPrinters {
		p.CUPSJobSemaphore = lib.NewSemaphore(cupsQueueSize)
		gcpPrintersByGCPID[p.GCPID] = p
	}

	gcpJobPollQuit := make(chan bool)
	printerPollQuit := make(chan bool)

	downloadSemaphore := lib.NewSemaphore(gcpMaxConcurrentDownload)
	jobStatsSemaphore := lib.NewSemaphore(1)

	jpi := time.Duration(jobPollInterval) * time.Second

	pm := PrinterManager{cups, gcp, gcpPrintersByGCPID, gcpJobPollQuit, printerPollQuit,
		downloadSemaphore, jobStatsSemaphore, 0, 0, cupsQueueSize, jpi, jobFullUsername}

	pm.syncPrinters()
	go pm.syncPrintersPeriodically(printerPollInterval)
	go pm.listenGCPJobs()

	return &pm, nil
}

func (pm *PrinterManager) Quit() {
	pm.printerPollQuit <- true
	<-pm.printerPollQuit
}

func (pm *PrinterManager) syncPrintersPeriodically(printerPollInterval uint) {
	interval := time.Duration(printerPollInterval) * time.Second
	for {
		select {
		case <-time.After(interval):
			pm.syncPrinters()
		case <-pm.printerPollQuit:
			pm.printerPollQuit <- true
			return
		}
	}
}

func printerMapToSlice(m map[string]lib.Printer) []lib.Printer {
	s := make([]lib.Printer, 0, len(m))
	for _, p := range m {
		s = append(s, p)
	}
	return s
}

func (pm *PrinterManager) syncPrinters() {
	glog.Info("Synchronizing printers, stand by")

	cupsPrinters, err := pm.cups.GetPrinters()
	if err != nil {
		glog.Errorf("Sync failed while calling GetPrinters(): %s", err)
		return
	}
	diffs := lib.DiffPrinters(cupsPrinters, printerMapToSlice(pm.gcpPrintersByGCPID))

	if diffs == nil {
		glog.Infof("Printers are already in sync; there are %d", len(cupsPrinters))
		return
	}

	ch := make(chan lib.Printer)
	for i := range diffs {
		go pm.applyDiff(&diffs[i], ch)
	}
	currentPrinters := make(map[string]lib.Printer)
	for _ = range diffs {
		p := <-ch
		if p.Name != "" {
			currentPrinters[p.GCPID] = p
		}
	}

	pm.gcpPrintersByGCPID = currentPrinters

	glog.Infof("Finished synchronizing %d printers", len(currentPrinters))
}

func (pm *PrinterManager) applyDiff(diff *lib.PrinterDiff, ch chan<- lib.Printer) {
	switch diff.Operation {
	case lib.RegisterPrinter:
		ppd, err := pm.cups.GetPPD(diff.Printer.Name)
		if err != nil {
			glog.Errorf("Failed to call GetPPD() while registering printer %s: %s",
				diff.Printer.Name, err)
			break
		}
		if err := pm.gcp.Register(&diff.Printer, ppd); err != nil {
			glog.Errorf("Failed to register printer %s: %s", diff.Printer.Name, err)
			break
		}
		glog.Infof("Registered %s", diff.Printer.Name)

		if pm.gcp.CanShare() {
			if err := pm.gcp.Share(diff.Printer.GCPID); err != nil {
				glog.Errorf("Failed to share printer %s: %s", diff.Printer.Name, err)
			} else {
				glog.Infof("Shared %s", diff.Printer.Name)
			}
		}

		diff.Printer.CUPSJobSemaphore = lib.NewSemaphore(pm.cupsQueueSize)

		ch <- diff.Printer
		return

	case lib.UpdatePrinter:
		var ppd string
		if diff.CapsHashChanged {
			var err error
			ppd, err = pm.cups.GetPPD(diff.Printer.Name)
			if err != nil {
				glog.Errorf("Failed to call GetPPD() while updating printer %s: %s",
					diff.Printer.Name, err)
				ch <- diff.Printer
				return
			}
		}

		if err := pm.gcp.Update(diff, ppd); err != nil {
			glog.Errorf("Failed to update a printer: %s", err)
		} else {
			glog.Infof("Updated %s", diff.Printer.Name)
		}

		ch <- diff.Printer
		return

	case lib.DeletePrinter:
		if err := pm.gcp.Delete(diff.Printer.GCPID); err != nil {
			glog.Errorf("Failed to delete a printer %s: %s", diff.Printer.GCPID, err)
			break
		}
		glog.Infof("Deleted %s", diff.Printer.Name)

	case lib.LeavePrinter:
		glog.Infof("No change to %s", diff.Printer.Name)
		ch <- diff.Printer
		return
	}

	ch <- lib.Printer{}
}

func (pm *PrinterManager) listenGCPJobs() {
	ch := make(chan *lib.Job)
	go func() {
		for {
			jobs, err := pm.gcp.NextJobBatch()
			if err != nil {
				glog.Warningf("Error waiting for next printer: %s", err)
			}
			for _, job := range jobs {
				ch <- &job
			}
		}
	}()

	for {
		select {
		case job := <-ch:
			go pm.processJob(job)
		case <-pm.gcpJobPollQuit:
			pm.gcpJobPollQuit <- true
			return
		}
	}
}

func (pm *PrinterManager) incrementJobsProcessed(success bool) {
	pm.jobStatsSemaphore.Acquire()
	defer pm.jobStatsSemaphore.Release()

	if success {
		pm.jobsDone += 1
	} else {
		pm.jobsError += 1
	}
}

// 0) Gets a job's ticket (job options).
// 1) Downloads a new print job PDF to a temp file.
// 2) Creates a new job in CUPS.
// 3) Polls the CUPS job status to update the GCP job status.
// 4) Returns when the job status is DONE or ERROR.
// 5) Deletes temp file.
func (pm *PrinterManager) processJob(job *lib.Job) {
	glog.Infof("Received job %s", job.GCPJobID)

	printer, exists := pm.gcpPrintersByGCPID[job.GCPPrinterID]
	if !exists {
		msg := fmt.Sprintf("Failed to find GCP printer %s for job %s", job.GCPPrinterID, job.GCPJobID)
		glog.Error(msg)
		pm.gcp.Control(job.GCPJobID, lib.JobError, msg)
		pm.incrementJobsProcessed(false)
		return
	}

	options, err := pm.gcp.Ticket(job.TicketURL)
	if err != nil {
		msg := fmt.Sprintf("Failed to get a ticket for job %s: %s", job.GCPJobID, err)
		glog.Error(msg)
		pm.gcp.Control(job.GCPJobID, lib.JobError, msg)
		pm.incrementJobsProcessed(false)
		return
	}

	pdfFile, err := pm.cups.CreateTempFile()
	if err != nil {
		msg := fmt.Sprintf("Failed to create a temporary file for job %s: %s", job.GCPJobID, err)
		glog.Error(msg)
		pm.gcp.Control(job.GCPJobID, lib.JobError, msg)
		pm.incrementJobsProcessed(false)
		return
	}

	pm.downloadSemaphore.Acquire()
	t := time.Now()
	err = pm.gcp.Download(pdfFile, job.FileURL)
	dt := time.Now().Sub(t)
	pm.downloadSemaphore.Release()
	if err != nil {
		msg := fmt.Sprintf("Failed to download PDF for job %s: %s", job.GCPJobID, err)
		glog.Error(msg)
		pm.gcp.Control(job.GCPJobID, lib.JobError, msg)
		pm.incrementJobsProcessed(false)
		return
	}

	glog.Infof("Downloaded job %s in %s", job.GCPJobID, dt.String())
	pdfFile.Close()
	defer os.Remove(pdfFile.Name())

	ownerID := job.OwnerID
	if !pm.jobFullUsername {
		ownerID = strings.Split(ownerID, "@")[0]
	}

	cupsJobID, err := pm.cups.Print(printer.Name, pdfFile.Name(), "gcp:"+job.GCPJobID, ownerID, options)
	if err != nil {
		msg := fmt.Sprintf("Failed to send job %s to CUPS: %s", job.GCPJobID, err)
		glog.Error(msg)
		pm.gcp.Control(job.GCPJobID, lib.JobError, msg)
		pm.incrementJobsProcessed(false)
		return
	}

	glog.Infof("Submitted GCP job %s as CUPS job %d", job.GCPJobID, cupsJobID)

	status := ""
	message := ""

	for _ = range time.Tick(pm.jobPollInterval) {
		latestStatus, latestMessage, err := pm.cups.GetJobStatus(cupsJobID)
		if err != nil {
			msg := fmt.Sprintf("Failed to get status of CUPS job %d: %s", cupsJobID, err)
			glog.Error(msg)
			pm.gcp.Control(job.GCPJobID, lib.JobError, msg)
			pm.incrementJobsProcessed(false)
			return
		}

		if latestStatus.GCPStatus() != status || latestMessage != message {
			status = latestStatus.GCPStatus()
			message = latestMessage
			pm.gcp.Control(job.GCPJobID, status, message)
			glog.Infof("Job %s status is now: %s", job.GCPJobID, status)
		}

		if latestStatus.GCPStatus() != lib.JobInProgress {
			if latestStatus.GCPStatus() == lib.JobDone {
				pm.incrementJobsProcessed(true)
			} else {
				pm.incrementJobsProcessed(false)
			}
			return
		}
	}
}

func (pm *PrinterManager) GetJobStats() (uint, uint, error) {
	var processed, processing uint

	for _, printer := range pm.gcpPrintersByGCPID {
		processing += printer.CUPSJobSemaphore.Count()
	}

	return processed, processing, nil
}
