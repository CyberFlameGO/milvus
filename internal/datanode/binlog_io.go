// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package datanode

import (
	"bytes"
	"context"
	"errors"
	"path"
	"strconv"

	"github.com/milvus-io/milvus/internal/kv"
	"github.com/milvus-io/milvus/internal/log"
	"github.com/milvus-io/milvus/internal/storage"

	"github.com/milvus-io/milvus/internal/proto/datapb"
	"github.com/milvus-io/milvus/internal/proto/etcdpb"

	"go.uber.org/zap"
)

var (
	errUploadToBlobStorage     = errors.New("upload to blob storage wrong")
	errDownloadFromBlobStorage = errors.New("download from blob storage wrong")
)

type downloader interface {
	// load downloads binlogs from blob storage for given paths.
	//  The paths are 1 group of binlog paths generated by 1 `Serialize`.
	//
	// download downloads insert-binlogs, stats-binlogs, and delta-binlogs.
	download(ctx context.Context, paths []string) ([]*Blob, error)
}

type uploader interface {
	// upload saves InsertData and DeleteData into blob storage.
	//  stats-binlogs are generated from InsertData.
	upload(ctx context.Context, segID, partID UniqueID, iData *InsertData, dData *DeleteData, meta *etcdpb.CollectionMeta) (*cpaths, error)
}

type binlogIO struct {
	kv.BaseKV
	allocatorInterface
}

var _ downloader = (*binlogIO)(nil)
var _ uploader = (*binlogIO)(nil)

func (b *binlogIO) download(ctx context.Context, paths []string) ([]*Blob, error) {
	var err error = errStart

	r := make(chan []string)
	go func(r chan<- []string) {
		var vs []string
		for err != nil {
			select {

			case <-ctx.Done():
				close(r)
				log.Debug("binlog download canceled by context done")
				return

			default:
				if err != errStart {
					log.Warn("Try multiloading again", zap.Strings("paths", paths))
				}
				vs, err = b.MultiLoad(paths)
			}
		}
		r <- vs
	}(r)

	vs, ok := <-r
	if !ok {
		return nil, errDownloadFromBlobStorage
	}

	rst := make([]*Blob, 0, len(vs))
	for _, vstr := range vs {
		b := bytes.NewBufferString(vstr)
		rst = append(rst, &Blob{Value: b.Bytes()})
	}

	return rst, nil
}

type cpaths struct {
	inPaths    []*datapb.FieldBinlog
	statsPaths []*datapb.FieldBinlog
	deltaInfo  *datapb.DeltaLogInfo
}

func (b *binlogIO) upload(
	ctx context.Context,
	segID, partID UniqueID,
	iData *InsertData,
	dData *DeleteData,
	meta *etcdpb.CollectionMeta) (*cpaths, error) {

	kvs, inpaths, statspaths, err := b.genInsertBlobs(iData, partID, segID, meta)
	if err != nil {
		log.Warn("generate insert blobs wrong", zap.Error(err))
		return nil, err
	}
	p := &cpaths{inpaths, statspaths, nil}

	// If there are delta logs
	if dData != nil {
		k, v, err := b.genDeltaBlobs(dData, meta.GetID(), partID, segID)
		if err != nil {
			log.Warn("generate delta blobs wrong", zap.Error(err))
			return nil, err
		}

		kvs[k] = bytes.NewBuffer(v).String()
		p.deltaInfo = &datapb.DeltaLogInfo{
			RecordEntries: uint64(len(v)),
			DeltaLogPath:  k,
		}
	}

	success := make(chan struct{})
	go func(success chan<- struct{}) {
		err := errStart
		for err != nil {
			select {
			case <-ctx.Done():
				close(success)
				log.Warn("ctx done when saving kvs to blob storage")
				return
			default:
				if err != errStart {
					log.Info("retry save binlogs")
				}
				err = b.MultiSave(kvs)
			}
		}
		success <- struct{}{}
	}(success)

	if _, ok := <-success; !ok {
		return nil, errUploadToBlobStorage
	}

	return p, nil
}

// returns key, value
func (b *binlogIO) genDeltaBlobs(data *DeleteData, collID, partID, segID UniqueID) (string, []byte, error) {
	dCodec := storage.NewDeleteCodec()

	blob, err := dCodec.Serialize(collID, partID, segID, data)
	if err != nil {
		return "", nil, err
	}

	k, err := b.genKey(true, collID, partID, segID)
	if err != nil {
		return "", nil, err
	}

	key := path.Join(Params.DeleteBinlogRootPath, k)

	return key, blob.GetValue(), nil
}

// return kvs, insert-paths, stats-paths
func (b *binlogIO) genInsertBlobs(data *InsertData, partID, segID UniqueID, meta *etcdpb.CollectionMeta) (map[string]string, []*datapb.FieldBinlog, []*datapb.FieldBinlog, error) {
	inCodec := storage.NewInsertCodec(meta)
	inlogs, statslogs, err := inCodec.Serialize(partID, segID, data)
	if err != nil {
		return nil, nil, nil, err
	}

	kvs := make(map[string]string, len(inlogs)+len(statslogs))
	inpaths := make([]*datapb.FieldBinlog, 0, len(inlogs))
	statspaths := make([]*datapb.FieldBinlog, 0, len(statslogs))

	notifyGenIdx := make(chan struct{})
	defer close(notifyGenIdx)

	generator, err := b.idxGenerator(len(inlogs)+len(statslogs), notifyGenIdx)
	if err != nil {
		return nil, nil, nil, err
	}

	for _, blob := range inlogs {
		fID, err := strconv.ParseInt(blob.GetKey(), 10, 64)
		if err != nil {
			log.Error("can not parse string to fieldID", zap.Error(err))
			return nil, nil, nil, err
		}
		k := JoinIDPath(meta.GetID(), partID, segID, fID, <-generator)
		key := path.Join(Params.InsertBinlogRootPath, k)

		kvs[key] = bytes.NewBuffer(blob.GetValue()).String()
		inpaths = append(inpaths, &datapb.FieldBinlog{
			FieldID: fID,
			Binlogs: []string{key},
		})
	}

	for _, blob := range statslogs {
		fID, err := strconv.ParseInt(blob.GetKey(), 10, 64)
		if err != nil {
			log.Error("can not parse string to fieldID", zap.Error(err))
			return nil, nil, nil, err
		}

		k := JoinIDPath(meta.GetID(), partID, segID, fID, <-generator)
		key := path.Join(Params.StatsBinlogRootPath, k)

		kvs[key] = bytes.NewBuffer(blob.GetValue()).String()
		statspaths = append(statspaths, &datapb.FieldBinlog{
			FieldID: fID,
			Binlogs: []string{key},
		})
	}

	return kvs, inpaths, statspaths, nil
}

func (b *binlogIO) idxGenerator(n int, done <-chan struct{}) (<-chan UniqueID, error) {

	idStart, _, err := b.allocIDBatch(uint32(n))
	if err != nil {
		return nil, err
	}

	rt := make(chan UniqueID)
	go func(rt chan<- UniqueID) {
		for i := 0; i < n; i++ {
			select {
			case <-done:
				close(rt)
				return
			case rt <- idStart + UniqueID(i):
			}
		}
		close(rt)
	}(rt)

	return rt, nil
}

func (b *binlogIO) close() {
	b.Close()
}
