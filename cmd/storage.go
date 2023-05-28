package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type ByStorageUsedEntity []EntityUsage

func (a ByStorageUsedEntity) Len() int           { return len(a) }
func (a ByStorageUsedEntity) Less(i, j int) bool { return a[i].StorageUsed > a[j].StorageUsed }
func (a ByStorageUsedEntity) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }

type EntityUsage struct {
	ID               string
	StorageUsed      int64
	Region           string
	IsVolume         bool
	AttachedInstance string // New field to store the attached EC2 instance ID
}

var (
	totalStorageUsed   int64
	entityMutex        sync.Mutex
	entities           []EntityUsage
	concurrentChannels = 100 // Set the default concurrent channel count

	ebsStorageUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "aws_ebs_storage_used",
			Help: "EBS storage used by volume",
		},
		[]string{"volume_id", "region", "attached_instance"}, // Added "attached_instance" label
	)

	snapshotStorageUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "aws_snapshot_storage_used",
			Help: "Snapshot storage used by snapshot",
		},
		[]string{"snapshot_id", "region", "attached_instance"}, // Added "attached_instance" label
	)

	totalStorageUsedMetric = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "aws_total_storage_used",
			Help: "Total storage used by all volumes and snapshots",
		},
	)
)

func main() {
	// Register the Prometheus metrics
	prometheus.MustRegister(ebsStorageUsed)
	prometheus.MustRegister(snapshotStorageUsed)
	prometheus.MustRegister(totalStorageUsedMetric)

	regions, err := GetAllAwsRegions()
	if err != nil {
		log.Fatalf("Failed to retrieve AWS regions: %v\n", err)
	}

	var wg sync.WaitGroup

	semaphore := make(chan struct{}, concurrentChannels)

	// Launch goroutines to query volumes and snapshots concurrently
	for _, region := range regions {
		wg.Add(2)

		go func(region string) {
			client, err := GetEc2Client(region)
			if err != nil {
				log.Printf("Failed to create EC2 client for region %s: %v\n", region, err)
				wg.Done()
				return
			}

			semaphore <- struct{}{} // Acquire a semaphore slot
			getEBSStorageUsed(client, region, &wg)
			<-semaphore // Release the semaphore slot
		}(*region.RegionName)

		go func(region string) {
			client, err := GetEc2Client(region)
			if err != nil {
				log.Printf("Failed to create EC2 client for region %s: %v\n", region, err)
				wg.Done()
				return
			}

			semaphore <- struct{}{} // Acquire a semaphore slot
			getSnapshotStorageUsed(client, region, &wg)
			<-semaphore // Release the semaphore slot
		}(*region.RegionName)
	}

	// Wait for all goroutines to complete
	wg.Wait()

	// Sort the entities by storage used in descending order
	entityMutex.Lock()
	sort.Sort(sort.Reverse(ByStorageUsedEntity(entities)))
	entityMutex.Unlock()
	totalStorageUsedMetric.Set(float64(totalStorageUsed))

	output := []map[string]interface{}{}

	for _, entity := range entities {
		entityType := "Volume"
		entityLink := ""
		if !entity.IsVolume {
			entityType = "Snapshot"
			entityLink = fmt.Sprintf("https://%s.console.aws.amazon.com/ec2/home?region=%s#SnapshotDetails:snapshotId=%s",
				strings.ToLower(entity.Region), entity.Region, entity.ID)
		} else if entity.AttachedInstance == "" {
			entityLink = fmt.Sprintf("https://%s.console.aws.amazon.com/ec2/home?region=%s#VolumeDetails:volumeId=%s",
				strings.ToLower(entity.Region), entity.Region, entity.ID)
		}

		attachedInstance := entity.AttachedInstance
		if attachedInstance == "" {
			attachedInstance = "Not Attached"
		}

		size := fmt.Sprintf("%.0f", float64(entity.StorageUsed)/(1024*1024*1024)) // Remove "GB" suffix

		output2 := fmt.Sprintf("Storage Used: %s, %s ID: %s, Region: %s, Attached Instance: %s",
			formatBytes(entity.StorageUsed), entityType, entity.ID, entity.Region, attachedInstance)

		if entityLink != "" {
			output2 += fmt.Sprintf(", Link: %s", entityLink)
		}
		fmt.Println(output2)

		output = append(output, map[string]interface{}{
			"Type":             entityType,
			"ID":               entity.ID,
			"StorageUsed":      size,
			"Region":           entity.Region,
			"AttachedInstance": attachedInstance,
			"Link":             entityLink,
		})
	}

	// Convert the output to JSON
	jsonOutput, err := json.MarshalIndent(output, "", "  ")
	if err != nil {
		log.Fatalf("Failed to convert output to JSON: %v\n", err)
	}

	// Write the JSON to a file
	err = os.WriteFile("storage.json", jsonOutput, 0o644)
	if err != nil {
		log.Fatalf("Failed to write JSON to file: %v\n", err)
	}

	totalStorageUsedTB := float64(totalStorageUsed) / (1024 * 1024 * 1024 * 1024)
	fmt.Printf("Total Storage Used: %.2f TB\n", totalStorageUsedTB)
	fmt.Printf("Output written to output.json\n")
	fmt.Printf("Listening for requests on localhost:8080/metrics...\n")

	// Start the Prometheus HTTP server
	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d bytes", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.0f GB", float64(bytes)/float64(div))
}

func getInstanceName(client *ec2.Client, instanceID string) string {
	params := &ec2.DescribeInstancesInput{
		InstanceIds: []string{instanceID},
	}
	resp, err := client.DescribeInstances(context.Background(), params)
	if err != nil {
		log.Printf("Failed to describe instances: %v\n", err)
		return ""
	}

	if len(resp.Reservations) == 0 || len(resp.Reservations[0].Instances) == 0 {
		log.Printf("No instance found with ID: %s\n", instanceID)
		return ""
	}

	instance := resp.Reservations[0].Instances[0]
	for _, tag := range instance.Tags {
		if *tag.Key == "Name" {
			return *tag.Value
		}
	}

	return ""
}

func getVolumeName(client *ec2.Client, volumeID string) string {
	params := &ec2.DescribeVolumesInput{
		VolumeIds: []string{volumeID},
	}
	resp, err := client.DescribeVolumes(context.Background(), params)
	if err != nil {
		log.Printf("Failed to describe volumes: %v\n", err)
		return ""
	}

	if len(resp.Volumes) == 0 {
		log.Printf("No volume found with ID: %s\n", volumeID)
		return ""
	}

	volume := resp.Volumes[0]
	for _, tag := range volume.Tags {
		if *tag.Key == "Name" {
			return *tag.Value
		}
	}

	return ""
}

func getEBSStorageUsed(client *ec2.Client, region string, wg *sync.WaitGroup) {
	defer wg.Done()

	log.Printf("Querying volumes in region: %s\n", region)
	params := &ec2.DescribeVolumesInput{}
	resp, err := client.DescribeVolumes(context.Background(), params)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == "InvalidVolume.NotFound" {
				// Handle the case when the volume does not exist
				log.Printf("Invalid volume ID: %s\n", aerr.Message())
				return
			}
		}

		log.Printf("Failed to describe volumes in region %s: %v\n", region, err)
		return
	}

	var volumes []EntityUsage

	for _, volume := range resp.Volumes {
		size := int64(*volume.Size) * 1024 * 1024 * 1024 // Convert from GB to bytes
		totalStorageUsed += size

		entity := EntityUsage{
			ID:               *volume.VolumeId,
			StorageUsed:      size,
			Region:           region,
			IsVolume:         true,
			AttachedInstance: "", // Initialize the attached instance ID as empty
		}

		if volume.Attachments != nil && len(volume.Attachments) > 0 {
			// Volume is attached to an instance
			entity.AttachedInstance = *volume.Attachments[0].InstanceId

			// Get instance name and replace instance ID with the tag "Name"
			instanceName := getInstanceName(client, entity.AttachedInstance)
			if instanceName != "" {
				entity.AttachedInstance = instanceName
			}
		}

		volumes = append(volumes, entity)

		ebsStorageUsed.WithLabelValues(*volume.VolumeId, region, entity.AttachedInstance).Set(float64(size))
	}

	entityMutex.Lock()
	entities = append(entities, volumes...)
	entityMutex.Unlock()
}

func getSnapshotStorageUsed(client *ec2.Client, region string, wg *sync.WaitGroup) {
	defer wg.Done()
	log.Printf("Querying snapshots in region: %s\n", region)

	params := &ec2.DescribeSnapshotsInput{
		OwnerIds: []string{"self"},
	}
	resp, err := client.DescribeSnapshots(context.Background(), params)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			if aerr.Code() == "InvalidSnapshot.NotFound" {
				// Handle the case when the snapshot does not exist
				log.Printf("Invalid snapshot ID: %s\n", aerr.Message())
				return
			}
		}

		log.Printf("Failed to describe snapshots in region %s: %v\n", region, err)
		return
	}

	var snapshots []EntityUsage

	for _, snapshot := range resp.Snapshots {
		size := int64(*snapshot.VolumeSize) * 1024 * 1024 * 1024 // Convert from GB to bytes
		totalStorageUsed += size

		entity := EntityUsage{
			ID:               *snapshot.SnapshotId,
			StorageUsed:      size,
			Region:           region,
			IsVolume:         false,
			AttachedInstance: "", // Snapshots are not attached to instances, so leave it empty
		}

		// Check if the snapshot has a "Name" tag
		for _, tag := range snapshot.Tags {
			if *tag.Key == "Name" {
				entity.AttachedInstance = *tag.Value
				break
			}
		}

		// If the snapshot doesn't have a "Name" tag, check if the volume still exists and get its name
		if entity.AttachedInstance == "" {
			volumeID := *snapshot.VolumeId
			volumeName := getVolumeName(client, volumeID)
			if volumeName != "" {
				entity.AttachedInstance = fmt.Sprintf("Volume: %s", volumeName)
			}
		}

		snapshots = append(snapshots, entity)

		snapshotStorageUsed.WithLabelValues(*snapshot.SnapshotId, region, entity.AttachedInstance).Set(float64(size))
	}

	entityMutex.Lock()
	entities = append(entities, snapshots...)
	entityMutex.Unlock()
}
