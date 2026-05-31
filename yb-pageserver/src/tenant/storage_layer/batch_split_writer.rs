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

