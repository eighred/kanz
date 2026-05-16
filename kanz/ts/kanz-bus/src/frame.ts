/** Wire frame helper — `unframe` for consumer-side decoding. */

import { fromBinary } from "@bufbuild/protobuf";
import type { Envelope } from "@kanz-eng/kanz-schemas/envelope/v1/envelope_pb.js";
import { EventFrameSchema } from "@kanz-eng/kanz-schemas/envelope/v1/event_frame_pb.js";

export interface Unframed {
  envelope: Envelope;
  payload: Uint8Array;
}

/**
 * Deserialize a `Message.body` into `(envelope, payload bytes)`.
 *
 * Consumers look up the payload schema in the registry (EVT-16) and
 * decode the payload against it. The envelope is returned as-is;
 * callers that want defensive validation should run {@link validate}
 * themselves.
 *
 * Throws on a malformed frame or a frame missing the envelope.
 */
export function unframe(body: Uint8Array): Unframed {
  let frame;
  try {
    frame = fromBinary(EventFrameSchema, body);
  } catch (e) {
    throw new Error(`frame unmarshal: ${(e as Error).message}`);
  }
  if (!frame.envelope) {
    throw new Error("frame missing envelope");
  }
  return { envelope: frame.envelope, payload: frame.payload };
}
