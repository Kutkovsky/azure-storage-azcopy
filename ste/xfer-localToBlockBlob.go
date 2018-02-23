package ste

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"github.com/Azure/azure-pipeline-go/pipeline"
	"github.com/Azure/azure-storage-azcopy/common"
	"github.com/Azure/azure-storage-blob-go/2016-05-31/azblob"
	"github.com/edsrzf/mmap-go"
	"net/url"
	"time"
)

type localToBlockBlob struct {
	// count the number of chunks that are done
	count uint32
}

// this function performs the setup for each transfer and schedules the corresponding chunkMsgs into the chunkChannel
func (localToBlockBlob localToBlockBlob) prologue(transfer TransferMsg, chunkChannel chan<- ChunkMsg) {

	jobInfo := transfer.getJobInfo()
	// step 1: create pipeline for the destination blob
	p := azblob.NewPipeline(azblob.NewAnonymousCredential(), azblob.PipelineOptions{
		Retry: azblob.RetryOptions{
			Policy:        azblob.RetryPolicyExponential,
			MaxTries:      5,
			TryTimeout:    time.Minute * 10,
			RetryDelay:    time.Second * 1,
			MaxRetryDelay: time.Second * 3,
		},
		Log: pipeline.LogOptions{
			Log: func(l pipeline.LogLevel, msg string) {
				jobInfo.Log(common.LogLevel(l), msg)
			},
			MinimumLevelToLog: func() pipeline.LogLevel {
				return pipeline.LogLevel(jobInfo.minimumLogLevel)
			},
		},
	})

	source, destination := transfer.SourceDestination()
	u, _ := url.Parse(destination)
	blobUrl := azblob.NewBlobURL(*u, p)

	// step 2: get the file size
	blobSize := int64(transfer.getSourceSize())

	// step 3: map in the file to upload before transferring chunks
	memoryMappedFile := openAndMemoryMapFile(source)

	// step 4: compute the number of blocks and create a slice to hold the blockIDs of each chunk
	chunkSize := int64(transfer.getBlockSize())

	numOfBlocks := transfer.getNumberOfChunks()

	blocksIds := make([]string, numOfBlocks)
	blockIdCount := int32(0)

	// step 5: go through the file and schedule chunk messages to upload each chunk
	for startIndex := int64(0); startIndex < blobSize; startIndex += chunkSize {
		adjustedChunkSize := chunkSize

		// compute actual size of the chunk
		if startIndex+chunkSize > blobSize {
			adjustedChunkSize = blobSize - startIndex
		}

		// schedule the chunk job/msg
		chunkChannel <- ChunkMsg{
			doTransfer: generateUploadFunc(
				transfer,
				blockIdCount, // this is the index of the chunk
				uint32(numOfBlocks),
				adjustedChunkSize,
				startIndex,
				blobUrl,
				memoryMappedFile,
				&blocksIds),
		}
		blockIdCount += 1
	}
}

// this generates a function which performs the uploading of a single chunk
func generateUploadFunc(t TransferMsg, chunkId int32, totalNumOfChunks uint32, chunkSize int64, startIndex int64, blobURL azblob.BlobURL,
										memoryMappedFile mmap.MMap, blockIds *[]string) chunkFunc {
	return func(workerId int) {
		transferIdentifierStr := t.getTransferIdentifierString()
		jobInfo := t.getJobInfo()
		if t.TransferContext.Err() != nil {
			jobInfo.Log(common.LogInfo, fmt.Sprintf("%s. Hence not picking up chunkId %d", t.getTransferIdentifierString(), chunkId))
			if t.incrementNumberOfChunksDone() == totalNumOfChunks {
				jobInfo.Log(common.LogInfo,
					fmt.Sprintf("worker %d is finalizing cancellation of job %s and part number %d",
						workerId, t.jobId, t.partNumber))
				//updateTransferStatus(jobId, partNum, transferId, common.TransferStatusFailed, jobsInfoMap)
				t.updateNumberOfTransferDone()
			}
		} else {

			// If there are more than one block for a transfer, then we need to upload each individually
			// and then we need to upload the block list
			if totalNumOfChunks > 1 {

				// step 1: generate block ID
				blockId := common.NewUUID().String()
				encodedBlockId := base64.StdEncoding.EncodeToString([]byte(blockId))

				// step 2: save the block ID into the list of block IDs
				(*blockIds)[chunkId] = encodedBlockId

				// step 3: perform put block
				blockBlobUrl := blobURL.ToBlockBlobURL()

				body := newRequestBodyPacer(bytes.NewReader(memoryMappedFile[startIndex:startIndex+chunkSize]), pc)
				putBlockResponse, err := blockBlobUrl.PutBlock(t.TransferContext, encodedBlockId, body, azblob.LeaseAccessConditions{})
				if err != nil {
					// cancel entire transfer because this chunk has failed
					t.TransferCancelFunc()
					jobInfo.Log(common.LogInfo,
						fmt.Sprintf("worker %d is canceling %s chunkId %d because startIndex of %d has failed",
							workerId, transferIdentifierStr, chunkId, startIndex))
					//fmt.Println("Worker", workerId, "is canceling CHUNK job with", transferIdentifierStr, "and chunkID", chunkId, "because startIndex of", startIndex, "has failed due to err", err)
					//updateChunkInfo(jobId, partNum, transferId, uint16(chunkId), ChunkTransferStatusFailed, jobsInfoMap)
					t.updateTransferStatus(common.TransferFailed)

					if t.incrementNumberOfChunksDone() == totalNumOfChunks {
						jobInfo.Log(common.LogInfo,
							fmt.Sprintf("worker %d is finalizing cancellation of %s",
								workerId, transferIdentifierStr))
						t.updateNumberOfTransferDone()

						err := memoryMappedFile.Unmap()
						if err != nil {
							jobInfo.Log(common.LogError,
								fmt.Sprintf("worker %v failed to conclude %s after processing chunkId %v",
									workerId, transferIdentifierStr, chunkId))
						}

					}
					return
				}

				if putBlockResponse != nil {
					putBlockResponse.Response().Body.Close()
				}

				//updateChunkInfo(jobId, partNum, transferId, uint16(chunkId), ChunkTransferStatusComplete, jobsInfoMap)
				realTimeThroughputCounter.updateCurrentBytes(chunkSize)

				// step 4: check if this is the last chunk
				if t.incrementNumberOfChunksDone() == totalNumOfChunks {
					// If the transfer gets cancelled before the putblock list
					if t.TransferContext.Err() != nil {
						t.updateNumberOfTransferDone()
						return
					}
					// step 5: this is the last block, perform EPILOGUE
					jobInfo.Log(common.LogInfo,
						fmt.Sprintf("worker %d is concluding download of %s after processing chunkId %d with blocklist %s",
							workerId, transferIdentifierStr, chunkId, *blockIds))
					//fmt.Println("Worker", workerId, "is concluding upload TRANSFER job with", transferIdentifierStr, "after processing chunkId", chunkId, "with blocklist", *blockIds)

					// fetching the blob http headers with content-type, content-encoding attributes
					blobHttpHeader := t.getBlobHttpHeaders(memoryMappedFile)

					// fetching the metadata passed with the JobPartOrder
					metaData := t.getJobPartMetaData()

					putBlockListResponse, err := blockBlobUrl.PutBlockList(t.TransferContext, *blockIds, blobHttpHeader, metaData, azblob.BlobAccessConditions{})
					if err != nil {
						jobInfo.Log(common.LogError,
							fmt.Sprintf("Worker %d failed to conclude Transfer job with %s after processing chunkId %d due to error %s",
								workerId, transferIdentifierStr, chunkId, string(err.Error())))
						t.updateTransferStatus(common.TransferFailed)
						t.updateNumberOfTransferDone()
						return
					}

					if putBlockListResponse != nil {
						putBlockListResponse.Response().Body.Close()
					}

					jobInfo.Log(common.LogInfo, fmt.Sprintf("%s has completed successfully", transferIdentifierStr))
					t.updateTransferStatus(common.TransferComplete)
					t.updateNumberOfTransferDone()

					err = memoryMappedFile.Unmap()
					if err != nil {
						jobInfo.Log(common.LogError,
							fmt.Sprintf("worker %v failed to conclude Transfer job with %v after processing chunkId %v",
								workerId, transferIdentifierStr, chunkId))
					}
				}
			} else {
				// If there is only one block for a transfer, then uploading block as a blob
				blockBlobUrl := blobURL.ToBlockBlobURL()

				blobHttpHeader := t.getBlobHttpHeaders(memoryMappedFile)

				// fetching the metadata passed with the JobPartOrder
				metaData := t.getJobPartMetaData()

				// reading the chunk contents
				body := newRequestBodyPacer(bytes.NewReader(memoryMappedFile[startIndex:startIndex+chunkSize]), pc)

				putblobResp, err := blockBlobUrl.PutBlob(t.TransferContext, body, blobHttpHeader, metaData, azblob.BlobAccessConditions{})

				// if the put blob is a failure, updating the transfer status to failed
				if err != nil {
					jobInfo.Log(common.LogInfo,
						fmt.Sprintf("put blob failed for %s failed and so cancelling the transfer", transferIdentifierStr))
					t.updateTransferStatus(common.TransferFailed)
				} else {
					// if the put blob is a success, updating the transfer status to success
					jobInfo.Log(common.LogInfo,
						fmt.Sprintf("put blob successful for %s by worked %d", transferIdentifierStr, workerId))
					t.updateTransferStatus(common.TransferComplete)
				}

				// updating number of transfers done for job part order
				t.updateNumberOfTransferDone()

				// closing the put blob response body
				if putblobResp != nil {
					putblobResp.Response().Body.Close()
				}

				err = memoryMappedFile.Unmap()
				if err != nil {
					jobInfo.Log(common.LogError,
						fmt.Sprintf("error mapping the memory map file for %s", transferIdentifierStr))
				}
			}
		}
	}
}
