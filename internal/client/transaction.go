package client

import (
	"fmt"
	"math"
	"sync"
	"time"

	"gopkg.in/cheggaaa/pb.v2"

	"github.com/clems4ever/go-graphkb/internal/knowledge"
	"github.com/clems4ever/go-graphkb/internal/schema"
	"github.com/clems4ever/go-graphkb/internal/utils"
)

// Transaction represent a transaction generating updates by diffing the provided graph against
// the previous version.
type Transaction struct {
	client *GraphClient

	currentGraph *knowledge.Graph

	// The graph being updated
	newGraph *knowledge.Graph
	binder   *knowledge.GraphBinder

	// Lock used when binding or relating assets
	mutex sync.Mutex

	// Number of parallel queries to the Graph API
	parallelization int

	// The number of items to send to the streaming API in one request
	chunkSize int
}

// Relate create a relation between two assets
func (cgt *Transaction) Relate(from string, relationType schema.RelationType, to string) {
	cgt.mutex.Lock()
	cgt.binder.Relate(from, relationType, to)
	cgt.mutex.Unlock()
}

// Bind bind one asset to an asset type from the schema
func (cgt *Transaction) Bind(asset string, assetType schema.AssetType) {
	cgt.mutex.Lock()
	cgt.binder.Bind(asset, assetType)
	cgt.mutex.Unlock()
}

// withRetryOnTooManyRequests helper retrying the function when too many request error has been received
func withRetryOnTooManyRequests(fn func() error, maxRetries int) error {
	trials := 0
	for {
		err := fn()
		if err != nil {
			backoffTime := time.Duration(int(math.Pow(1.01, float64(trials)))*15) * time.Second
			fmt.Printf("Sleeping for %d seconds\n", backoffTime/time.Second)
			time.Sleep(backoffTime)
		} else {
			return nil
		}
		trials++
		if trials == maxRetries {
			return fmt.Errorf("Too many retries... Aborting")
		}
	}
}

// Commit commit the transaction and gives ownership to the source for caching.
func (cgt *Transaction) Commit() (*knowledge.Graph, error) {
	sg := cgt.newGraph.ExtractSchema()

	fmt.Println("Start uploading the schema of the graph...")
	if err := cgt.client.UpdateSchema(sg); err != nil {
		return nil, fmt.Errorf("Unable to update the schema of the graph: %v", err)
	}

	fmt.Println("Finished uploading the schema of the graph...")

	bulk := knowledge.GenerateGraphUpdatesBulk(cgt.currentGraph, cgt.newGraph)

	fmt.Println("Start uploading the graph...")

	progress := pb.New(len(bulk.GetAssetRemovals()) + len(bulk.GetAssetUpserts()) + len(bulk.GetRelationRemovals()) + len(bulk.GetRelationUpserts()))
	defer progress.Finish()

	progress.Start()
	p := utils.NewWorkerPool(cgt.parallelization)
	defer p.Close()

	futures := make([]chan error, 0)

	chunkSize := cgt.chunkSize

	relationRemovalsChunks := utils.ChunkSlice(bulk.GetRelationRemovals(), chunkSize).([][]interface{})
	relationInsertionChunks := utils.ChunkSlice(bulk.GetRelationUpserts(), chunkSize).([][]interface{})
	assetRemovalsChunks := utils.ChunkSlice(bulk.GetAssetRemovals(), chunkSize).([][]interface{})
	assetInsertionChunks := utils.ChunkSlice(bulk.GetAssetUpserts(), chunkSize).([][]interface{})

	for _, rels := range relationRemovalsChunks {
		relations := []knowledge.Relation{}
		for _, r := range rels {
			relations = append(relations, r.(knowledge.Relation))
		}
		f := p.Exec(func() error {
			if err := withRetryOnTooManyRequests(func() error { return cgt.client.DeleteRelations(relations) }, 10); err != nil {
				return fmt.Errorf("Unable to remove the relations %v: %v", relations, err)
			}
			progress.Add(len(relations))
			return nil
		})
		futures = append(futures, f)
	}

	for _, ass := range assetInsertionChunks {
		assets := []knowledge.Asset{}
		for _, a := range ass {
			assets = append(assets, a.(knowledge.Asset))
		}
		f := p.Exec(func() error {
			if err := withRetryOnTooManyRequests(func() error { return cgt.client.InsertAssets(assets) }, 10); err != nil {
				return fmt.Errorf("Unable to upsert the asset %v: %v", assets, err)
			}
			progress.Add(len(assets))
			return nil
		})
		futures = append(futures, f)
	}

	// Wait for all futures to complete
	for _, f := range futures {
		err := <-f
		if err != nil {
			return nil, err
		}
	}

	futures = make([]chan error, 0)

	for _, ass := range assetRemovalsChunks {
		assets := []knowledge.Asset{}
		for _, a := range ass {
			assets = append(assets, a.(knowledge.Asset))
		}
		f := p.Exec(func() error {
			if err := withRetryOnTooManyRequests(func() error { return cgt.client.DeleteAssets(assets) }, 10); err != nil {
				return fmt.Errorf("Unable to remove the asset %v: %v", assets, err)
			}
			progress.Add(len(assets))
			return nil
		})
		futures = append(futures, f)
	}

	for _, rels := range relationInsertionChunks {
		relations := []knowledge.Relation{}
		for _, r := range rels {
			relations = append(relations, r.(knowledge.Relation))
		}
		f := p.Exec(func() error {
			if err := withRetryOnTooManyRequests(func() error { return cgt.client.InsertRelations(relations) }, 10); err != nil {
				return fmt.Errorf("Unable to upsert the relation %v: %v", relations, err)
			}
			progress.Add(len(relations))
			return nil
		})
		futures = append(futures, f)
	}

	// Wait for all futures to complete
	for _, f := range futures {
		err := <-f
		if err != nil {
			return nil, err
		}
	}

	fmt.Println("Finished uploading the graph...")

	g := cgt.newGraph
	cgt.newGraph = knowledge.NewGraph()
	return g, nil // give ownership of the transaction graph so that it can be cached if needed
}