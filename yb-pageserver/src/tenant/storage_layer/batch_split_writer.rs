use std::future::Future;
use std::ops::Range;
use std::sync::Arc;

use bytes::Bytes;
use pageserver_api::key::{KEY_SIZE, Key};
use tokio_util::sync::CancellationToken;
use utils::id::TimelineId;
use utils::lsn::Lsn;
use utils::shard::TenantShardId;
use wal_decoder::models::value::Value;

use super::errors::PutError;
use super::{
    DeltaLayerWriter, ImageLayerWriter, PersistentLayerDesc, PersistentLayerKey, ResidentLayer,
};
use crate::config::PageServerConf;
use crate::context::RequestContext;
use crate::tenant::Timeline;
use crate::tenant::storage_layer::Layer;

pub(crate) enum BatchWriterResult {
    Produced(ResidentLayer),
    Discarded(PersistentLayerKey),
}

enum LayerWriterWrapper {
    Image(ImageLayerWriter),
    Delta(DeltaLayerWriter),
}

/// An layer writer that takes unfinished layers and finish them atomically.
#[must_use]
pub struct BatchLayerWriter {
    generated_layer_writers: Vec<(LayerWriterWrapper, PersistentLayerKey)>,
    conf: &'static PageServerConf,
}

impl BatchLayerWriter {
    pub fn new(conf: &'static PageServerConf) -> Self {
        Self {
            generated_layer_writers: Vec::new(),
            conf,
        }
    }

    pub fn add_unfinished_image_writer(
        &mut self,
        writer: ImageLayerWriter,
        key_range: Range<Key>,
        lsn: Lsn,
    ) {
        self.generated_layer_writers.push((
            LayerWriterWrapper::Image(writer),
            PersistentLayerKey {
                key_range,
                lsn_range: PersistentLayerDesc::image_layer_lsn_range(lsn),
                is_delta: false,
            },
        ));
    }

    pub fn add_unfinished_delta_writer(
        &mut self,
        writer: DeltaLayerWriter,
        key_range: Range<Key>,
        lsn_range: Range<Lsn>,
    ) {
        self.generated_layer_writers.push((
            LayerWriterWrapper::Delta(writer),
            PersistentLayerKey {
                key_range,
                lsn_range,
                is_delta: true,
            },
        ));
    }

    pub(crate) async fn finish(
        self,
        tline: &Arc<Timeline>,
        ctx: &RequestContext,
    ) -> anyhow::Result<Vec<ResidentLayer>> {
        let res = self
            .finish_with_discard_fn(tline, ctx, |_| async { false })
            .await?;
        let mut output = Vec::new();
        for r in res {
            if let BatchWriterResult::Produced(layer) = r {
                output.push(layer);
            }
        }
        Ok(output)
    }

    pub(crate) async fn finish_with_discard_fn<D, F>(
        self,
        tline: &Arc<Timeline>,
        ctx: &RequestContext,
        discard_fn: D,
    ) -> anyhow::Result<Vec<BatchWriterResult>>
    where
        D: Fn(&PersistentLayerKey) -> F,
        F: Future<Output = bool>,
    {
        let Self {
            generated_layer_writers,
            ..
        } = self;
        let clean_up_layers = |generated_layers: Vec<BatchWriterResult>| {
            for produced_layer in generated_layers {
                if let BatchWriterResult::Produced(resident_layer) = produced_layer {
                    let layer: Layer = resident_layer.into();
                    layer.delete_on_drop();
                }
            }
        };
        // BEGIN: catch every error and do the recovery in the below section
        let mut generated_layers: Vec<BatchWriterResult> = Vec::new();
        for (inner, layer_key) in generated_layer_writers {
            if discard_fn(&layer_key).await {
                generated_layers.push(BatchWriterResult::Discarded(layer_key));
            } else {
                let res = match inner {
                    LayerWriterWrapper::Delta(writer) => {
                        writer.finish(layer_key.key_range.end, ctx).await
                    }
                    LayerWriterWrapper::Image(writer) => {
                        writer
                            .finish_with_end_key(layer_key.key_range.end, ctx)
                            .await
                    }
                };
                let layer = match res {
                    Ok((desc, path)) => {
                        match Layer::finish_creating(self.conf, tline, desc, &path) {
                            Ok(layer) => layer,
                            Err(e) => {
                                tokio::fs::remove_file(&path).await.ok();
                                clean_up_layers(generated_layers);
                                return Err(e);
                            }
                        }
                    }
                    Err(e) => {
                        // Image/DeltaLayerWriter::finish will clean up the temporary layer if anything goes wrong,
                        // so we don't need to remove the layer we just failed to create by ourselves.
                        clean_up_layers(generated_layers);
                        return Err(e);
                    }
                };
                generated_layers.push(BatchWriterResult::Produced(layer));
            }
        }
        // END: catch every error and do the recovery in the above section
        Ok(generated_layers)
    }

    pub fn pending_layer_num(&self) -> usize {
        self.generated_layer_writers.len()
    }
}

/// An image writer that takes images and produces multiple image layers.
#[must_use]
pub struct SplitImageLayerWriter<'a> {
    inner: Option<ImageLayerWriter>,
    target_layer_size: u64,
    lsn: Lsn,
    conf: &'static PageServerConf,
    timeline_id: TimelineId,
    tenant_shard_id: TenantShardId,
    batches: BatchLayerWriter,
    start_key: Key,
    gate: &'a utils::sync::gate::Gate,
    cancel: CancellationToken,
}

impl<'a> SplitImageLayerWriter<'a> {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        conf: &'static PageServerConf,
        timeline_id: TimelineId,
        tenant_shard_id: TenantShardId,
        start_key: Key,
        lsn: Lsn,
        target_layer_size: u64,
        gate: &'a utils::sync::gate::Gate,
        cancel: CancellationToken,
    ) -> Self {
        Self {
            target_layer_size,
            inner: None,
            conf,
            timeline_id,
            tenant_shard_id,
            batches: BatchLayerWriter::new(conf),
            lsn,
            start_key,
            gate,
            cancel,
        }
    }

    pub async fn put_image(
        &mut self,
        key: Key,
        img: Bytes,
        ctx: &RequestContext,
    ) -> Result<(), PutError> {
        if self.inner.is_none() {
            self.inner = Some(
                ImageLayerWriter::new(
                    self.conf,
                    self.timeline_id,
                    self.tenant_shard_id,
                    &(self.start_key..Key::MAX),
                    self.lsn,
                    self.gate,
                    self.cancel.clone(),
                    ctx,
                )
                .await
                .map_err(PutError::Other)?,
            );
        }

        let inner = self.inner.as_mut().unwrap();

        // The current estimation is an upper bound of the space that the key/image could take
        // because we did not consider compression in this estimation. The resulting image layer
        // could be smaller than the target size.
        let addition_size_estimation = KEY_SIZE as u64 + img.len() as u64;
        if inner.num_keys() >= 1
            && inner.estimated_size() + addition_size_estimation >= self.target_layer_size
        {
            let next_image_writer = ImageLayerWriter::new(
                self.conf,
                self.timeline_id,
                self.tenant_shard_id,
                &(key..Key::MAX),
                self.lsn,
                self.gate,
                self.cancel.clone(),
                ctx,
            )
            .await
            .map_err(PutError::Other)?;
            let prev_image_writer = std::mem::replace(inner, next_image_writer);
            self.batches.add_unfinished_image_writer(
                prev_image_writer,
                self.start_key..key,
                self.lsn,
            );
            self.start_key = key;
        }
        inner.put_image(key, img, ctx).await
    }

    pub(crate) async fn finish_with_discard_fn<D, F>(
        self,
        tline: &Arc<Timeline>,
        ctx: &RequestContext,
        end_key: Key,
        discard_fn: D,
    ) -> anyhow::Result<Vec<BatchWriterResult>>
    where
        D: Fn(&PersistentLayerKey) -> F,
        F: Future<Output = bool>,
    {
        let Self {
            mut batches, inner, ..
        } = self;
        if let Some(inner) = inner {
            if inner.num_keys() != 0 {
                batches.add_unfinished_image_writer(inner, self.start_key..end_key, self.lsn);
            }
        }
        batches.finish_with_discard_fn(tline, ctx, discard_fn).await
    }

    #[cfg(test)]
    pub(crate) async fn finish(
        self,
        tline: &Arc<Timeline>,
        ctx: &RequestContext,
        end_key: Key,
    ) -> anyhow::Result<Vec<BatchWriterResult>> {
        self.finish_with_discard_fn(tline, ctx, end_key, |_| async { false })
            .await
    }
}

/// A delta writer that takes key-lsn-values and produces multiple delta layers.
///
/// Note that if updates of a single key exceed the target size limit, all of the updates will be batched
/// into a single file. This behavior might change in the future. For reference, the legacy compaction algorithm
/// will split them into multiple files based on size.
#[must_use]
pub struct SplitDeltaLayerWriter<'a> {
    inner: Option<(Key, DeltaLayerWriter)>,
    target_layer_size: u64,
    conf: &'static PageServerConf,
    timeline_id: TimelineId,
    tenant_shard_id: TenantShardId,
    lsn_range: Range<Lsn>,
    last_key_written: Key,
    batches: BatchLayerWriter,
    gate: &'a utils::sync::gate::Gate,
    cancel: CancellationToken,
}

impl<'a> SplitDeltaLayerWriter<'a> {
    pub fn new(
        conf: &'static PageServerConf,
        timeline_id: TimelineId,
        tenant_shard_id: TenantShardId,
        lsn_range: Range<Lsn>,
        target_layer_size: u64,
        gate: &'a utils::sync::gate::Gate,
        cancel: CancellationToken,
    ) -> Self {
        Self {
            target_layer_size,
            inner: None,
            conf,
            timeline_id,
            tenant_shard_id,
            lsn_range,
            last_key_written: Key::MIN,
            batches: BatchLayerWriter::new(conf),
            gate,
            cancel,
        }
    }

    pub async fn put_value(
        &mut self,
        key: Key,
        lsn: Lsn,
        val: Value,
        ctx: &RequestContext,
    ) -> Result<(), PutError> {
        // The current estimation is key size plus LSN size plus value size estimation. This is not an accurate
        // number, and therefore the final layer size could be a little bit larger or smaller than the target.
        //
        // Also, keep all updates of a single key in a single file. TODO: split them using the legacy compaction
        // strategy. https://github.com/neondatabase/neon/issues/8837

        if self.inner.is_none() {
            self.inner = Some((
                key,
                DeltaLayerWriter::new(
                    self.conf,
                    self.timeline_id,
                    self.tenant_shard_id,
                    key,
                    self.lsn_range.clone(),
                    self.gate,
                    self.cancel.clone(),
                    ctx,
                )
                .await
                .map_err(PutError::Other)?,
            ));
        }
        let (_, inner) = self.inner.as_mut().unwrap();

        let addition_size_estimation = KEY_SIZE as u64 + 8 /* LSN u64 size */ + 80 /* value size estimation */;
        if inner.num_keys() >= 1
            && inner.estimated_size() + addition_size_estimation >= self.target_layer_size
        {
            if key != self.last_key_written {
                let next_delta_writer = DeltaLayerWriter::new(
                    self.conf,
                    self.timeline_id,
                    self.tenant_shard_id,
                    key,
                    self.lsn_range.clone(),
                    self.gate,
                    self.cancel.clone(),
                    ctx,
                )
                .await
                .map_err(PutError::Other)?;
                let (start_key, prev_delta_writer) =
                    self.inner.replace((key, next_delta_writer)).unwrap();
                self.batches.add_unfinished_delta_writer(
                    prev_delta_writer,
                    start_key..key,
                    self.lsn_range.clone(),
                );
            } else if inner.estimated_size() >= self.conf.s3_upload_limit_bytes {
                // We have to produce a very large file b/c a key is updated too often.
                return Err(PutError::Other(anyhow::anyhow!(
                    "a single key is updated too often: key={}, estimated_size={}, and the layer file cannot be produced",
                    key,
                    inner.estimated_size()
                )));
            }
        }
        self.last_key_written = key;
        let (_, inner) = self.inner.as_mut().unwrap();
        inner.put_value(key, lsn, val, ctx).await
    }

    pub(crate) async fn finish_with_discard_fn<D, F>(
        self,
        tline: &Arc<Timeline>,
        ctx: &RequestContext,
        discard_fn: D,
    ) -> anyhow::Result<Vec<BatchWriterResult>>
    where
        D: Fn(&PersistentLayerKey) -> F,
        F: Future<Output = bool>,
    {
        let Self {
            mut batches, inner, ..
        } = self;
        if let Some((start_key, writer)) = inner {
            if writer.num_keys() != 0 {
                let end_key = self.last_key_written.next();
                batches.add_unfinished_delta_writer(
                    writer,
                    start_key..end_key,
                    self.lsn_range.clone(),
                );
            }
        }
        batches.finish_with_discard_fn(tline, ctx, discard_fn).await
    }

    #[cfg(test)]
    pub(crate) async fn finish(
        self,
        tline: &Arc<Timeline>,
        ctx: &RequestContext,
    ) -> anyhow::Result<Vec<BatchWriterResult>> {
        self.finish_with_discard_fn(tline, ctx, |_| async { false })
            .await
    }
}

