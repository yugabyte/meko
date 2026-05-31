use std::ops::Range;
use std::sync::Arc;

use pageserver_api::key::Key;
use utils::lsn::Lsn;

use super::{
    DeltaLayerWriter, ImageLayerWriter, PersistentLayerDesc, PersistentLayerKey, ResidentLayer,
};
use crate::config::PageServerConf;
use crate::context::RequestContext;
use crate::tenant::Timeline;
use crate::tenant::storage_layer::Layer;

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
        let Self {
            generated_layer_writers,
            ..
        } = self;
        let clean_up_layers = |generated_layers: Vec<ResidentLayer>| {
            for resident_layer in generated_layers {
                let layer: Layer = resident_layer.into();
                layer.delete_on_drop();
            }
        };
        let mut generated_layers: Vec<ResidentLayer> = Vec::new();
        for (inner, layer_key) in generated_layer_writers {
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
                Ok((desc, path)) => match Layer::finish_creating(self.conf, tline, desc, &path) {
                    Ok(layer) => layer,
                    Err(e) => {
                        tokio::fs::remove_file(&path).await.ok();
                        clean_up_layers(generated_layers);
                        return Err(e);
                    }
                },
                Err(e) => {
                    clean_up_layers(generated_layers);
                    return Err(e);
                }
            };
            generated_layers.push(layer);
        }
        Ok(generated_layers)
    }

    pub fn pending_layer_num(&self) -> usize {
        self.generated_layer_writers.len()
    }
}

