//! Canonical normalizer shared with the actual typed venue-client consumer.
use crate::Error;
use prost_reflect::MessageDescriptor;
pub(crate) fn normalize(desc: MessageDescriptor, raw: &[u8]) -> Result<Vec<u8>, Error> {
    kanz_proto_wire::normalize(desc, raw).map_err(Into::into)
}
#[cfg(test)]
mod tests {
    use super::*;
    use prost_reflect::DescriptorPool;
    fn decode(name: &str, raw: &[u8]) -> Vec<u8> {
        let pool = DescriptorPool::decode(kanz_order_core::proto::FILE_DESCRIPTOR_SET).unwrap();
        normalize(pool.get_message_by_name(name).unwrap(), raw).unwrap()
    }
    #[test]
    fn known_noncanonical_values_normalize_unknown_values_do_not() {
        assert_eq!(
            decode(
                "common.v1.Decimal",
                &[0xa0, 0x86, 0x00, 0x81, 0, 8, 0x82, 0]
            ),
            vec![8, 2, 0xa0, 6, 0x81, 0]
        );
    }
    #[test]
    fn duplicate_embedded_messages_merge_unknowns_in_encounter_order() {
        assert_eq!(
            decode(
                "common.v1.Money",
                &[10, 5, 8, 2, 0xa0, 6, 1, 10, 5, 16, 4, 0x98, 6, 2]
            ),
            vec![10, 10, 8, 2, 16, 4, 0xa0, 6, 1, 0x98, 6, 2]
        );
    }
    #[test]
    fn repeated_messages_keep_separate_unknowns() {
        let raw = [
            26, 6, 10, 1, b'a', 0xa0, 6, 1, 26, 6, 10, 1, b'b', 0xa0, 6, 2,
        ];
        assert_eq!(decode("order.v1.ExecutionRecoveryHistory", &raw), raw);
    }
    #[test]
    fn malformed_payload_refuses() {
        let pool = DescriptorPool::decode(kanz_order_core::proto::FILE_DESCRIPTOR_SET).unwrap();
        assert!(normalize(
            pool.get_message_by_name("common.v1.Money").unwrap(),
            &[10, 4, 8]
        )
        .is_err());
    }
}
