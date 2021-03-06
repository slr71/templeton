package elasticsearch

import (
	"fmt"
	"io"

	"github.com/cyverse-de/logcabin"

	"github.com/cyverse-de/esutils"
	"gopkg.in/olivere/elastic.v5"

	"context"

	"github.com/cyverse-de/templeton/database"
	"github.com/cyverse-de/templeton/model"
)

var (
	// knownTypes is a mapping which stores known types to index
	knownTypes = map[string]bool{
		"file":   true,
		"folder": true,
	}
)

// Elasticer is a type used to interact with Elasticsearch
type Elasticer struct {
	es       *elastic.Client
	baseURL  string
	user     string
	password string
	index    string
}

// NewElasticer returns a pointer to an Elasticer instance that has already tested its connection
// by making a WaitForStatus call to the configured Elasticsearch cluster
func NewElasticer(elasticsearchBase string, user string, password string, elasticsearchIndex string) (*Elasticer, error) {
	c, err := elastic.NewSimpleClient(elastic.SetURL(elasticsearchBase), elastic.SetBasicAuth(user, password))

	if err != nil {
		return nil, err
	}

	return &Elasticer{es: c, baseURL: elasticsearchBase, index: elasticsearchIndex}, nil
}

func (e *Elasticer) Close() {
	e.es.Stop()
}

func (e *Elasticer) NewBulkIndexer(bulkSize int) *esutils.BulkIndexer {
	return esutils.NewBulkIndexer(e.es, bulkSize)
}

func (e *Elasticer) PurgeType(d *database.Databaser, indexer *esutils.BulkIndexer, t string) error {
	scanner := e.es.Scroll(e.index).Type(t).Scroll("1m")

	for {
		docs, err := scanner.Do(context.TODO())
		if err == io.EOF {
			logcabin.Info.Printf("Finished all rows for purge of %s.", t)
			break
		}
		if err != nil {
			return err
		}

		if docs.TotalHits() > 0 {
			for _, hit := range docs.Hits.Hits {
				avus, err := d.GetObjectAVUs(hit.Id)
				if err != nil {
					logcabin.Error.Printf("Error processing %s/%s: %s", t, hit.Id, err)
					continue
				}
				if len(avus) == 0 {
					logcabin.Info.Printf("Deleting %s/%s", t, hit.Id)
					req := elastic.NewBulkDeleteRequest().Index(e.index).Type(t).Routing(hit.Id).Id(hit.Id)
					err = indexer.Add(req)
					if err != nil {
						logcabin.Error.Printf("Error enqueuing delete of %s/%s: %s", t, hit.Id, err)
					}
				}
			}
		}
	}
	return nil
}

// PurgeIndex walks an index querying a database, deleting those which should not exist
func (e *Elasticer) PurgeIndex(d *database.Databaser) {
	indexer := e.NewBulkIndexer(1000)
	defer indexer.Flush()

	err := e.PurgeType(d, indexer, "file_metadata")
	if err != nil {
		logcabin.Error.Fatal(err)
		return
	}

	err = e.PurgeType(d, indexer, "folder_metadata")
	if err != nil {
		logcabin.Error.Fatal(err)
		return
	}
}

// IndexEverything creates a bulk indexer and takes a database, and iterates to index its contents
func (e *Elasticer) IndexEverything(d *database.Databaser) {
	indexer := e.NewBulkIndexer(1000)
	defer indexer.Flush()

	cursor, err := d.GetAllObjects()
	if err != nil {
		logcabin.Error.Fatal(err)
	}
	defer cursor.Close()

	for {
		avus, err := cursor.Next()
		if err == database.EOS {
			logcabin.Info.Print("Done all rows, finishing.")
			break
		}
		if err != nil {
			logcabin.Error.Print(err)
			break
		}

		formatted, err := model.AVUsToIndexedObject(avus)
		if err != nil {
			logcabin.Error.Print(err)
			break
		}

		if knownTypes[avus[0].TargetType] {
			indexedType := fmt.Sprintf("%s_metadata", avus[0].TargetType)
			logcabin.Info.Printf("Indexing %s/%s", indexedType, formatted.ID)

			req := elastic.NewBulkIndexRequest().Index(e.index).Type(indexedType).Parent(formatted.ID).Id(formatted.ID).Doc(formatted)
			err = indexer.Add(req)
			if err != nil {
				logcabin.Error.Print(err)
				break
			}
		}
	}
}

func (e *Elasticer) Reindex(d *database.Databaser) {
	e.PurgeIndex(d)
	e.IndexEverything(d)
}

func (e *Elasticer) DeleteOne(id string) {
	logcabin.Info.Printf("Deleting metadata for %s", id)
	_, fileErr := e.es.Delete().Index(e.index).Type("file_metadata").Parent(id).Id(id).Do(context.TODO())
	_, folderErr := e.es.Delete().Index(e.index).Type("folder_metadata").Parent(id).Id(id).Do(context.TODO())
	if fileErr != nil && folderErr != nil {
		logcabin.Error.Printf("Error deleting file metadata for %s: %s", id, fileErr)
		logcabin.Error.Printf("Error deleting folder metadata for %s: %s", id, folderErr)
	}
	return
}

// IndexOne takes a database and one ID and reindexes that one entity. It should not die or throw errors.
func (e *Elasticer) IndexOne(d *database.Databaser, id string) {
	avus, err := d.GetObjectAVUs(id)
	if err != nil {
		logcabin.Error.Print(err)
		return
	}

	formatted, err := model.AVUsToIndexedObject(avus)
	if err == model.ErrNoAVUs {
		e.DeleteOne(id)
		return
	}
	if err != nil {
		logcabin.Error.Print(err)
		return
	}

	if knownTypes[avus[0].TargetType] {
		indexedType := fmt.Sprintf("%s_metadata", avus[0].TargetType)
		logcabin.Info.Printf("Indexing %s/%s", indexedType, formatted.ID)
		_, err = e.es.Index().Index(e.index).Type(indexedType).Parent(formatted.ID).Id(formatted.ID).BodyJson(formatted).Do(context.TODO())
		if err != nil {
			logcabin.Error.Print(err)
		}
	}
	return
}
