package mongorestore

import (
	"fmt"
	"github.com/mongodb/mongo-tools/common/db"
	"github.com/mongodb/mongo-tools/common/intents"
	"github.com/mongodb/mongo-tools/common/log"
	"github.com/mongodb/mongo-tools/common/progress"
	"gopkg.in/mgo.v2/bson"
	"io"
	"io/ioutil"
	"os"
	"strings"
	"time"
)

const (
	progressBarLength   = 24
	progressBarWaitTime = time.Second * 3

	insertBufferFactor = 16
)

// RestoreIntents iterates through all of the intents stored in the IntentManager, and restores them.
func (restore *MongoRestore) RestoreIntents() error {

	// start up the progress bar manager
	restore.progressManager = progress.NewProgressBarManager(log.Writer(0), progressBarWaitTime)
	restore.progressManager.Start()
	defer restore.progressManager.Stop()

	log.Logf(log.DebugLow, "restoring up to %v collections in parallel", restore.OutputOptions.NumParallelCollections)

	if restore.OutputOptions.NumParallelCollections > 0 {
		resultChan := make(chan error)

		// start a goroutine for each job thread
		for i := 0; i < restore.OutputOptions.NumParallelCollections; i++ {
			go func(id int) {
				log.Logf(log.DebugHigh, "starting restore routine with id=%v", id)
				for {
					intent := restore.manager.Pop()
					if intent == nil {
						log.Logf(log.DebugHigh, "ending restore routine with id=%v, no more work to do", id)
						resultChan <- nil // done
						return
					}
					err := restore.RestoreIntent(intent)
					if err != nil {
						resultChan <- fmt.Errorf("%v: %v", intent.Namespace(), err)
						return
					}
					restore.manager.Finish(intent)
				}
			}(i)
		}

		// wait until all goroutines are done or one of them errors out
		for i := 0; i < restore.OutputOptions.NumParallelCollections; i++ {
			if err := <-resultChan; err != nil {
				return err
			}
		}
		return nil
	}

	// single-threaded
	for intent := restore.manager.Pop(); intent != nil; intent = restore.manager.Pop() {
		err := restore.RestoreIntent(intent)
		if err != nil {
			return fmt.Errorf("%v: %v", intent.Namespace(), err)
		}
		restore.manager.Finish(intent)
	}
	return nil
}

// RestoreIntent attempts to restore a given intent into MongoDB.
func (restore *MongoRestore) RestoreIntent(intent *intents.Intent) error {

	collectionExists, err := restore.CollectionExists(intent)
	if err != nil {
		return fmt.Errorf("error reading database: %v", err)
	}

	if restore.safety == nil && !restore.OutputOptions.Drop && collectionExists {
		log.Logf(log.Always, "restoring to existing collection %v without dropping", intent.Namespace())
		log.Log(log.Always, "Important: restored data will be inserted without raising errors; check your server log")
	}

	if restore.OutputOptions.Drop {
		if collectionExists {
			if strings.HasPrefix(intent.C, "system.") {
				log.Logf(log.Always, "cannot drop system collection %v, skipping", intent.Namespace())
			} else {
				log.Logf(log.Info, "dropping collection %v before restoring", intent.Namespace())
				err = restore.DropCollection(intent)
				if err != nil {
					return err // no context needed
				}
				collectionExists = false
			}
		} else {
			log.Logf(log.DebugLow, "collection %v doesn't exist, skipping drop command", intent.Namespace())
		}
	}

	var options bson.D
	var indexes []IndexDocument

	// get indexes from system.indexes dump if we have it but don't have metadata files
	if intent.MetadataPath == "" {
		if _, ok := restore.collectionIndexes[intent.DB]; ok {
			if indexes, ok = restore.collectionIndexes[intent.DB][intent.C]; ok {
				log.Logf(log.Always, "no metadata file; falling back to system.indexes")
			}
		}
	}

	// first create the collection with options from the metadata file
	if intent.MetadataPath != "" {
		err = intent.OpenMetadata(intent)
		if err != nil {
			return err
		}
		log.Logf(log.Always, "reading metadata file from %v", intent.MetadataPath)
		metadata, err := ioutil.ReadAll(intent.MetadataFile)
		if err != nil {
			return fmt.Errorf("error reading metadata file %v: %v", intent.MetadataPath, err)
		}
		options, indexes, err = restore.MetadataFromJSON(metadata)
		if err != nil {
			return fmt.Errorf("error parsing metadata file %v: %v", intent.MetadataPath, err)
		}
		if !restore.OutputOptions.NoOptionsRestore {
			if options != nil {
				if !collectionExists {
					log.Logf(log.Info, "creating collection %v using options from metadata", intent.Namespace())
					err = restore.CreateCollection(intent, options)
					if err != nil {
						return fmt.Errorf("error creating collection %v: %v", intent.Namespace(), err)
					}
				} else {
					log.Logf(log.Info, "collection %v already exists", intent.Namespace())
				}
			} else {
				log.Log(log.Info, "no collection options to restore")
			}
		} else {
			log.Log(log.Info, "skipping options restoration")
		}
	}

	// then do bson
	if intent.BSONPath != "" {
		err = intent.OpenIntent(intent)
		if err != nil {
			return err
		}

		log.Logf(log.Always, "restoring %v from file %v", intent.Namespace(), intent.BSONPath)
		var size int64

		bsonSource := db.NewDecodedBSONSource(db.NewBSONSource(intent.BSONFile))
		defer bsonSource.Close()

		err = restore.RestoreCollectionToDB(intent.DB, intent.C, bsonSource, size)
		if err != nil {
			return fmt.Errorf("error restoring from %v: %v", intent.BSONPath, err)
		}
	}

	// finally, add indexes
	if len(indexes) > 0 && !restore.OutputOptions.NoIndexRestore {
		log.Logf(log.Always, "restoring indexes for collection %v from metadata", intent.Namespace())
		err = restore.CreateIndexes(intent, indexes)
		if err != nil {
			return fmt.Errorf("error creating indexes for %v: %v", intent.Namespace(), err)
		}
	} else {
		log.Log(log.Always, "no indexes to restore")
	}

	log.Logf(log.Always, "finished restoring %v", intent.Namespace())
	return nil
}

// RestoreCollectionToDB pipes the given BSON data into the database.
func (restore *MongoRestore) RestoreCollectionToDB(dbName, colName string,
	bsonSource *db.DecodedBSONSource, fileSize int64) error {

	session, err := restore.SessionProvider.GetSession()
	if err != nil {
		return fmt.Errorf("error establishing connection: %v", err)
	}
	session.SetSafe(restore.safety)
	session.SetSocketTimeout(0)
	defer session.Close()

	collection := session.DB(dbName).C(colName)

	watchProgressor := progress.NewCounter(fileSize)
	bar := &progress.Bar{
		Name:      fmt.Sprintf("%v.%v", dbName, colName),
		Watching:  watchProgressor,
		BarLength: progressBarLength,
		IsBytes:   true,
	}
	restore.progressManager.Attach(bar)
	defer restore.progressManager.Detach(bar)

	maxInsertWorkers := restore.OutputOptions.NumInsertionWorkers
	if restore.OutputOptions.MaintainInsertionOrder {
		maxInsertWorkers = 1
	}
	docChan := make(chan bson.Raw, insertBufferFactor)
	resultChan := make(chan error, maxInsertWorkers)

	go func() {
		doc := bson.Raw{}
		for bsonSource.Next(&doc) {
			rawBytes := make([]byte, len(doc.Data))
			copy(rawBytes, doc.Data)
			docChan <- bson.Raw{Data: rawBytes}
		}
		close(docChan)
	}()

	log.Logf(log.DebugLow, "using %v insertion workers", maxInsertWorkers)

	for i := 0; i < maxInsertWorkers; i++ {
		go func() {
			// get a session copy for each insert worker
			s := session.Copy()
			defer s.Close()

			coll := collection.With(s)
			bulk := db.NewBufferedBulkInserter(
				coll, restore.ToolOptions.BulkBufferSize, !restore.OutputOptions.StopOnError)
			for rawDoc := range docChan {
				if restore.objCheck {
					err := bson.Unmarshal(rawDoc.Data, &bson.D{})
					if err != nil {
						resultChan <- fmt.Errorf("invalid object: %v", err)
						return
					}
				}
				if err := bulk.Insert(rawDoc); err != nil {
					if db.IsConnectionError(err) || restore.OutputOptions.StopOnError {
						// Propagate this error, since it's either a fatal connection error
						// or the user has turned on --stopOnError
						resultChan <- err
					} else {
						// Otherwise just log the error but don't propagate it.
						log.Logf(log.Always, "error: %v", err)
					}
				}
				watchProgressor.Inc(int64(len(rawDoc.Data)))
			}
			err := bulk.Flush()
			if err != nil {
				if !db.IsConnectionError(err) && !restore.OutputOptions.StopOnError {
					// Suppress this error since it's not a severe connection error and
					// the user has not specified --stopOnError
					log.Logf(log.Always, "error: %v", err)
					err = nil
				}
			}
			resultChan <- err
			return
		}()

		// sleep to prevent all threads from inserting at the same time at start
		time.Sleep(time.Duration(i) * 10 * time.Millisecond)
	}

	// wait until all insert jobs finish
	for done := 0; done < maxInsertWorkers; done++ {
		err := <-resultChan
		if err != nil {
			return fmt.Errorf("insertion error: %v", err)
		}
	}
	// final error check
	if err = bsonSource.Err(); err != nil {
		return fmt.Errorf("reading bson input: %v", err)
	}
	return nil
}
