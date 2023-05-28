package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"
)

const cacheFilePath = "regions_cache.json"

func CreateConfig(region string) (aws.Config, error) {
	cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		return aws.Config{}, err
	}
	return cfg, nil
}

func GetEc2Client(region string) (*ec2.Client, error) {
	config, err := CreateConfig(region)
	if err != nil {
		return nil, err
	}
	// Create an EC2 client
	return ec2.NewFromConfig(config), nil
}

func GetAllAwsRegions() ([]types.Region, error) {
	// Return cached regions if available
	if cachedRegions, err := readRegionsFromCache(); err == nil {
		return cachedRegions, nil
	}

	client, err := GetEc2Client("us-west-2")
	if err != nil {
		panic(err)
	}

	// Get a list of all AWS regions
	resp, err := client.DescribeRegions(context.Background(), nil)
	if err != nil {
		return nil, fmt.Errorf("failed to describe AWS regions: %w", err)
	}

	regions := resp.Regions

	// Cache the regions to disk
	if err := writeRegionsToCache(regions); err != nil {
		fmt.Printf("Warning: Failed to write regions cache to disk: %v\n", err)
	}

	return regions, nil
}

func readRegionsFromCache() ([]types.Region, error) {
	cacheFile, err := os.Open(cacheFilePath)
	if err != nil {
		return nil, err
	}
	defer cacheFile.Close()

	var cachedRegions []types.Region
	err = json.NewDecoder(cacheFile).Decode(&cachedRegions)
	if err != nil {
		return nil, err
	}

	return cachedRegions, nil
}

func writeRegionsToCache(regions []types.Region) error {
	cacheFileDir := filepath.Dir(cacheFilePath)
	if err := os.MkdirAll(cacheFileDir, 0o755); err != nil {
		return err
	}

	cacheFile, err := os.Create(cacheFilePath)
	if err != nil {
		return err
	}
	defer cacheFile.Close()

	encodedRegions, err := json.Marshal(regions)
	if err != nil {
		return err
	}

	err = os.WriteFile(cacheFilePath, encodedRegions, 0o644)
	if err != nil {
		return err
	}

	return nil
}
