/*
 * MinIO Cloud Storage, (C) 2020 MinIO, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	minio "github.com/minio/minio-go/v7"
	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/encrypt"
	"github.com/minio/minio-go/v7/pkg/tags"
	"github.com/minio/minio/cmd/crypto"
	xhttp "github.com/minio/minio/cmd/http"
	"github.com/minio/minio/cmd/logger"
	"github.com/minio/minio/pkg/bucket/bandwidth"
	"github.com/minio/minio/pkg/bucket/replication"
	"github.com/minio/minio/pkg/event"
	iampolicy "github.com/minio/minio/pkg/iam/policy"
	"github.com/minio/minio/pkg/madmin"
)

// gets replication config associated to a given bucket name.
func getReplicationConfig(ctx context.Context, bucketName string) (rc *replication.Config, err error) {
	if globalIsGateway {
		objAPI := newObjectLayerFn()
		if objAPI == nil {
			return nil, errServerNotInitialized
		}

		return nil, BucketReplicationConfigNotFound{Bucket: bucketName}
	}

	return globalBucketMetadataSys.GetReplicationConfig(ctx, bucketName)
}

// validateReplicationDestination returns error if replication destination bucket missing or not configured
// It also returns true if replication destination is same as this server.
func validateReplicationDestination(ctx context.Context, bucket string, rCfg *replication.Config) (bool, error) {
	arn, err := madmin.ParseARN(rCfg.RoleArn)
	if err != nil {
		return false, BucketRemoteArnInvalid{}
	}
	if arn.Type != madmin.ReplicationService {
		return false, BucketRemoteArnTypeInvalid{}
	}
	clnt := globalBucketTargetSys.GetRemoteTargetClient(ctx, rCfg.RoleArn)
	if clnt == nil {
		return false, BucketRemoteTargetNotFound{Bucket: bucket}
	}
	if found, _ := clnt.BucketExists(ctx, rCfg.GetDestination().Bucket); !found {
		return false, BucketRemoteDestinationNotFound{Bucket: rCfg.GetDestination().Bucket}
	}
	if ret, err := globalBucketObjectLockSys.Get(bucket); err == nil {
		if ret.LockEnabled {
			lock, _, _, _, err := clnt.GetObjectLockConfig(ctx, rCfg.GetDestination().Bucket)
			if err != nil || lock != "Enabled" {
				return false, BucketReplicationDestinationMissingLock{Bucket: rCfg.GetDestination().Bucket}
			}
		}
	}
	// validate replication ARN against target endpoint
	c, ok := globalBucketTargetSys.arnRemotesMap[rCfg.RoleArn]
	if ok {
		if c.EndpointURL().String() == clnt.EndpointURL().String() {
			sameTarget, _ := isLocalHost(clnt.EndpointURL().Hostname(), clnt.EndpointURL().Port(), globalMinioPort)
			return sameTarget, nil
		}
	}
	return false, BucketRemoteTargetNotFound{Bucket: bucket}
}

func mustReplicateWeb(ctx context.Context, r *http.Request, bucket, object string, meta map[string]string, replStatus string, permErr APIErrorCode) bool {
	if permErr != ErrNone {
		return false
	}
	return mustReplicater(ctx, bucket, object, meta, replStatus)
}

// mustReplicate returns true if object meets replication criteria.
func mustReplicate(ctx context.Context, r *http.Request, bucket, object string, meta map[string]string, replStatus string) bool {
	if s3Err := isPutActionAllowed(ctx, getRequestAuthType(r), bucket, "", r, iampolicy.GetReplicationConfigurationAction); s3Err != ErrNone {
		return false
	}
	return mustReplicater(ctx, bucket, object, meta, replStatus)
}

// mustReplicater returns true if object meets replication criteria.
func mustReplicater(ctx context.Context, bucket, object string, meta map[string]string, replStatus string) bool {
	if globalIsGateway {
		return false
	}
	if rs, ok := meta[xhttp.AmzBucketReplicationStatus]; ok {
		replStatus = rs
	}
	if replication.StatusType(replStatus) == replication.Replica {
		return false
	}
	cfg, err := getReplicationConfig(ctx, bucket)
	if err != nil {
		return false
	}
	opts := replication.ObjectOpts{
		Name: object,
		SSEC: crypto.SSEC.IsEncrypted(meta),
	}
	tagStr, ok := meta[xhttp.AmzObjectTagging]
	if ok {
		opts.UserTags = tagStr
	}
	return cfg.Replicate(opts)
}

// returns true if any of the objects being deleted qualifies for replication.
func hasReplicationRules(ctx context.Context, bucket string, objects []ObjectToDelete) bool {
	c, err := getReplicationConfig(ctx, bucket)
	if err != nil || c == nil {
		return false
	}
	for _, obj := range objects {
		if c.HasActiveRules(obj.ObjectName, true) {
			return true
		}
	}
	return false
}

// returns whether object version is a deletemarker and if object qualifies for replication
func checkReplicateDelete(ctx context.Context, bucket string, dobj ObjectToDelete, oi ObjectInfo, gerr error) (dm, replicate bool) {
	rcfg, err := getReplicationConfig(ctx, bucket)
	if err != nil || rcfg == nil {
		return false, false
	}
	// when incoming delete is removal of a delete marker( a.k.a versioned delete),
	// GetObjectInfo returns extra information even though it returns errFileNotFound
	if gerr != nil {
		validReplStatus := false
		switch oi.ReplicationStatus {
		case replication.Pending, replication.Complete, replication.Failed:
			validReplStatus = true
		}
		if oi.DeleteMarker && validReplStatus {
			return oi.DeleteMarker, true
		}
		return oi.DeleteMarker, false
	}
	opts := replication.ObjectOpts{
		Name:         dobj.ObjectName,
		SSEC:         crypto.SSEC.IsEncrypted(oi.UserDefined),
		UserTags:     oi.UserTags,
		DeleteMarker: true,
		VersionID:    dobj.VersionID,
	}
	return oi.DeleteMarker, rcfg.Replicate(opts)
}

// replicate deletes to the designated replication target if replication configuration
// has delete marker replication or delete replication (MinIO extension to allow deletes where version id
// is specified) enabled.
// Similar to bucket replication for PUT operation, soft delete (a.k.a setting delete marker) and
// permanent deletes (by specifying a version ID in the delete operation) have three states "Pending", "Complete"
// and "Failed" to mark the status of the replication of "DELETE" operation. All failed operations can
// then be retried by healing. In the case of permanent deletes, until the replication is completed on the
// target cluster, the object version is marked deleted on the source and hidden from listing. It is permanently
// deleted from the source when the VersionPurgeStatus changes to "Complete", i.e after replication succeeds
// on target.
func replicateDelete(ctx context.Context, dobj DeletedObjectVersionInfo, objectAPI ObjectLayer) {
	bucket := dobj.Bucket
	rcfg, err := getReplicationConfig(ctx, bucket)
	if err != nil || rcfg == nil {
		return
	}
	tgt := globalBucketTargetSys.GetRemoteTargetClient(ctx, rcfg.RoleArn)
	if tgt == nil {
		return
	}
	versionID := dobj.DeleteMarkerVersionID
	if versionID == "" {
		versionID = dobj.VersionID
	}
	rmErr := tgt.RemoveObject(ctx, rcfg.GetDestination().Bucket, dobj.ObjectName, miniogo.RemoveObjectOptions{
		VersionID: versionID,
		Internal: miniogo.AdvancedRemoveOptions{
			ReplicationDeleteMarker: dobj.DeleteMarkerVersionID != "",
			ReplicationMTime:        dobj.DeleteMarkerMTime.Time,
			ReplicationStatus:       miniogo.ReplicationStatusReplica,
		},
	})

	replicationStatus := dobj.DeleteMarkerReplicationStatus
	versionPurgeStatus := dobj.VersionPurgeStatus

	if rmErr != nil {
		if dobj.VersionID == "" {
			replicationStatus = string(replication.Failed)
		} else {
			versionPurgeStatus = Failed
		}
	} else {
		if dobj.VersionID == "" {
			replicationStatus = string(replication.Complete)
		} else {
			versionPurgeStatus = Complete
		}
	}
	var eventName = event.ObjectReplicationComplete
	if replicationStatus == string(replication.Failed) || versionPurgeStatus == Failed {
		eventName = event.ObjectReplicationFailed
	}
	objInfo := ObjectInfo{
		Name:               dobj.ObjectName,
		DeleteMarker:       dobj.DeleteMarker,
		VersionID:          versionID,
		ReplicationStatus:  replication.StatusType(dobj.DeleteMarkerReplicationStatus),
		VersionPurgeStatus: versionPurgeStatus,
	}

	eventArg := &eventArgs{
		BucketName: bucket,
		Object:     objInfo,
		Host:       "Internal: [Replication]",
		EventName:  eventName,
	}
	sendEvent(*eventArg)

	// Update metadata on the delete marker or purge permanent delete if replication success.
	if _, err = objectAPI.DeleteObject(ctx, bucket, dobj.ObjectName, ObjectOptions{
		VersionID:                     versionID,
		DeleteMarker:                  dobj.DeleteMarker,
		DeleteMarkerReplicationStatus: replicationStatus,
		Versioned:                     globalBucketVersioningSys.Enabled(bucket),
		VersionPurgeStatus:            versionPurgeStatus,
		VersionSuspended:              globalBucketVersioningSys.Suspended(bucket),
	}); err != nil {
		logger.LogIf(ctx, fmt.Errorf("Unable to update replication metadata for %s/%s %s: %w", bucket, dobj.ObjectName, dobj.VersionID, err))
	}
}

func getCopyObjMetadata(oi ObjectInfo, dest replication.Destination) map[string]string {
	meta := make(map[string]string, len(oi.UserDefined))
	for k, v := range oi.UserDefined {
		if k == xhttp.AmzBucketReplicationStatus {
			continue
		}
		if strings.HasPrefix(strings.ToLower(k), ReservedMetadataPrefixLower) {
			continue
		}
		meta[k] = v
	}
	if oi.ContentEncoding != "" {
		meta[xhttp.ContentEncoding] = oi.ContentEncoding
	}
	if oi.ContentType != "" {
		meta[xhttp.ContentType] = oi.ContentType
	}
	tag, err := tags.ParseObjectTags(oi.UserTags)
	if err != nil {
		return nil
	}
	if tag != nil {
		meta[xhttp.AmzObjectTagging] = tag.String()
		meta[xhttp.AmzTagDirective] = "REPLACE"
	}
	sc := dest.StorageClass
	if sc == "" {
		sc = oi.StorageClass
	}
	meta[xhttp.AmzStorageClass] = sc
	if oi.UserTags != "" {
		meta[xhttp.AmzObjectTagging] = oi.UserTags
	}
	meta[xhttp.MinIOSourceMTime] = oi.ModTime.Format(time.RFC3339Nano)
	meta[xhttp.MinIOSourceETag] = oi.ETag
	meta[xhttp.AmzBucketReplicationStatus] = replication.Replica.String()
	return meta
}

func putReplicationOpts(ctx context.Context, dest replication.Destination, objInfo ObjectInfo) (putOpts miniogo.PutObjectOptions) {
	meta := make(map[string]string)
	for k, v := range objInfo.UserDefined {
		if k == xhttp.AmzBucketReplicationStatus {
			continue
		}
		if strings.HasPrefix(strings.ToLower(k), ReservedMetadataPrefixLower) {
			continue
		}
		meta[k] = v
	}
	tag, err := tags.ParseObjectTags(objInfo.UserTags)
	if err != nil {
		return
	}
	sc := dest.StorageClass
	if sc == "" {
		sc = objInfo.StorageClass
	}
	putOpts = miniogo.PutObjectOptions{
		UserMetadata:    meta,
		UserTags:        tag.ToMap(),
		ContentType:     objInfo.ContentType,
		ContentEncoding: objInfo.ContentEncoding,
		StorageClass:    sc,
		Internal: miniogo.AdvancedPutOptions{
			SourceVersionID:   objInfo.VersionID,
			ReplicationStatus: miniogo.ReplicationStatusReplica,
			SourceMTime:       objInfo.ModTime,
			SourceETag:        objInfo.ETag,
		},
	}
	if mode, ok := objInfo.UserDefined[xhttp.AmzObjectLockMode]; ok {
		rmode := miniogo.RetentionMode(mode)
		putOpts.Mode = rmode
	}
	if retainDateStr, ok := objInfo.UserDefined[xhttp.AmzObjectLockRetainUntilDate]; ok {
		rdate, err := time.Parse(time.RFC3339Nano, retainDateStr)
		if err != nil {
			return
		}
		putOpts.RetainUntilDate = rdate
	}
	if lhold, ok := objInfo.UserDefined[xhttp.AmzObjectLockLegalHold]; ok {
		putOpts.LegalHold = miniogo.LegalHoldStatus(lhold)
	}
	if crypto.S3.IsEncrypted(objInfo.UserDefined) {
		putOpts.ServerSideEncryption = encrypt.NewSSE()
	}

	return
}

type replicationAction string

const (
	replicateMetadata replicationAction = "metadata"
	replicateNone     replicationAction = "none"
	replicateAll      replicationAction = "all"
)

// returns replicationAction by comparing metadata between source and target
func getReplicationAction(oi1 ObjectInfo, oi2 minio.ObjectInfo) replicationAction {
	// needs full replication
	if oi1.ETag != oi2.ETag ||
		oi1.VersionID != oi2.VersionID ||
		oi1.Size != oi2.Size ||
		oi1.DeleteMarker != oi2.IsDeleteMarker {
		return replicateAll
	}

	if !oi1.ModTime.Equal(oi2.LastModified) ||
		oi1.ContentType != oi2.ContentType ||
		oi1.StorageClass != oi2.StorageClass {
		return replicateMetadata
	}
	if oi1.ContentEncoding != "" {
		enc, ok := oi2.UserMetadata[xhttp.ContentEncoding]
		if !ok || enc != oi1.ContentEncoding {
			return replicateMetadata
		}
	}
	for k, v := range oi2.UserMetadata {
		oi2.Metadata[k] = []string{v}
	}
	if len(oi2.Metadata) != len(oi1.UserDefined) {
		return replicateMetadata
	}
	for k1, v1 := range oi1.UserDefined {
		if v2, ok := oi2.Metadata[k1]; !ok || v1 != strings.Join(v2, "") {
			return replicateMetadata
		}
	}
	t, _ := tags.MapToObjectTags(oi2.UserTags)
	if t.String() != oi1.UserTags {
		return replicateMetadata
	}
	return replicateNone
}

// replicateObject replicates the specified version of the object to destination bucket
// The source object is then updated to reflect the replication status.
func replicateObject(ctx context.Context, objInfo ObjectInfo, objectAPI ObjectLayer) {
	bucket := objInfo.Bucket
	object := objInfo.Name

	cfg, err := getReplicationConfig(ctx, bucket)
	if err != nil {
		logger.LogIf(ctx, err)
		return
	}
	tgt := globalBucketTargetSys.GetRemoteTargetClient(ctx, cfg.RoleArn)
	if tgt == nil {
		logger.LogIf(ctx, fmt.Errorf("failed to get target for bucket:%s arn:%s", bucket, cfg.RoleArn))
		return
	}
	gr, err := objectAPI.GetObjectNInfo(ctx, bucket, object, nil, http.Header{}, readLock, ObjectOptions{
		VersionID: objInfo.VersionID,
	})
	if err != nil {
		return
	}
	objInfo = gr.ObjInfo
	size, err := objInfo.GetActualSize()
	if err != nil {
		logger.LogIf(ctx, err)
		gr.Close()
		return
	}

	dest := cfg.GetDestination()
	if dest.Bucket == "" {
		gr.Close()
		return
	}

	rtype := replicateAll
	oi, err := tgt.StatObject(ctx, dest.Bucket, object, miniogo.StatObjectOptions{VersionID: objInfo.VersionID})
	if err == nil {
		rtype = getReplicationAction(objInfo, oi)
		if rtype == replicateNone {
			gr.Close()
			// object with same VersionID already exists, replication kicked off by
			// PutObject might have completed.
			return
		}
	}

	target, err := globalBucketMetadataSys.GetBucketTarget(bucket, cfg.RoleArn)
	if err != nil {
		logger.LogIf(ctx, fmt.Errorf("failed to get target for replication bucket:%s cfg:%s err:%s", bucket, cfg.RoleArn, err))
		return
	}
	putOpts := putReplicationOpts(ctx, dest, objInfo)
	replicationStatus := replication.Complete

	// Setup bandwidth throttling
	peers, _ := globalEndpoints.peers()
	totalNodesCount := len(peers)
	if totalNodesCount == 0 {
		totalNodesCount = 1 // For standalone erasure coding
	}
	b := target.BandwidthLimit / int64(totalNodesCount)
	var headerSize int
	for k, v := range putOpts.Header() {
		headerSize += len(k) + len(v)
	}
	r := bandwidth.NewMonitoredReader(ctx, globalBucketMonitor, objInfo.Bucket, objInfo.Name, gr, headerSize, b, target.BandwidthLimit)
	if rtype == replicateAll {
		_, err = tgt.PutObject(ctx, dest.Bucket, object, r, size, "", "", putOpts)
	} else {
		// replicate metadata for object tagging/copy with metadata replacement
		dstOpts := miniogo.PutObjectOptions{Internal: miniogo.AdvancedPutOptions{SourceVersionID: objInfo.VersionID}}
		_, err = tgt.CopyObject(ctx, dest.Bucket, object, dest.Bucket, object, getCopyObjMetadata(objInfo, dest), dstOpts)
	}

	r.Close()
	if err != nil {
		replicationStatus = replication.Failed
	}
	objInfo.UserDefined[xhttp.AmzBucketReplicationStatus] = replicationStatus.String()
	if objInfo.UserTags != "" {
		objInfo.UserDefined[xhttp.AmzObjectTagging] = objInfo.UserTags
	}

	// FIXME: add support for missing replication events
	// - event.ObjectReplicationNotTracked
	// - event.ObjectReplicationMissedThreshold
	// - event.ObjectReplicationReplicatedAfterThreshold
	var eventName = event.ObjectReplicationComplete
	if replicationStatus == replication.Failed {
		eventName = event.ObjectReplicationFailed
	}
	sendEvent(eventArgs{
		EventName:  eventName,
		BucketName: bucket,
		Object:     objInfo,
		Host:       "Internal: [Replication]",
	})
	objInfo.metadataOnly = true // Perform only metadata updates.
	if _, err = objectAPI.CopyObject(ctx, bucket, object, bucket, object, objInfo, ObjectOptions{
		VersionID: objInfo.VersionID,
	}, ObjectOptions{
		VersionID: objInfo.VersionID,
	}); err != nil {
		logger.LogIf(ctx, fmt.Errorf("Unable to update replication metadata for %s: %s", objInfo.VersionID, err))
	}
}

// filterReplicationStatusMetadata filters replication status metadata for COPY
func filterReplicationStatusMetadata(metadata map[string]string) map[string]string {
	// Copy on write
	dst := metadata
	var copied bool
	delKey := func(key string) {
		if _, ok := metadata[key]; !ok {
			return
		}
		if !copied {
			dst = make(map[string]string, len(metadata))
			for k, v := range metadata {
				dst[k] = v
			}
			copied = true
		}
		delete(dst, key)
	}

	delKey(xhttp.AmzBucketReplicationStatus)
	return dst
}

// DeletedObjectVersionInfo has info on deleted object
type DeletedObjectVersionInfo struct {
	DeletedObject
	Bucket string
}
type replicationState struct {
	// add future metrics here
	replicaCh       chan ObjectInfo
	replicaDeleteCh chan DeletedObjectVersionInfo
}

func (r *replicationState) queueReplicaTask(oi ObjectInfo) {
	if r == nil {
		return
	}
	select {
	case r.replicaCh <- oi:
	default:
	}
}

func (r *replicationState) queueReplicaDeleteTask(doi DeletedObjectVersionInfo) {
	if r == nil {
		return
	}
	select {
	case r.replicaDeleteCh <- doi:
	default:
	}
}

var (
	globalReplicationState *replicationState
	// TODO: currently keeping it conservative
	// but eventually can be tuned in future,
	// take only half the CPUs for replication
	// conservatively.
	globalReplicationConcurrent = runtime.GOMAXPROCS(0) / 2
)

func newReplicationState() *replicationState {

	// fix minimum concurrent replication to 1 for single CPU setup
	if globalReplicationConcurrent == 0 {
		globalReplicationConcurrent = 1
	}
	rs := &replicationState{
		replicaCh:       make(chan ObjectInfo, 10000),
		replicaDeleteCh: make(chan DeletedObjectVersionInfo, 10000),
	}
	go func() {
		<-GlobalContext.Done()
		close(rs.replicaCh)
		close(rs.replicaDeleteCh)
	}()
	return rs
}

// addWorker creates a new worker to process tasks
func (r *replicationState) addWorker(ctx context.Context, objectAPI ObjectLayer) {
	// Add a new worker.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case oi, ok := <-r.replicaCh:
				if !ok {
					return
				}
				replicateObject(ctx, oi, objectAPI)
			case doi, ok := <-r.replicaDeleteCh:
				if !ok {
					return
				}
				replicateDelete(ctx, doi, objectAPI)
			}
		}
	}()
}

func initBackgroundReplication(ctx context.Context, objectAPI ObjectLayer) {
	if globalReplicationState == nil {
		return
	}

	// Start with globalReplicationConcurrent.
	for i := 0; i < globalReplicationConcurrent; i++ {
		globalReplicationState.addWorker(ctx, objectAPI)
	}
}
