pub mod lake {
    pub mod v1 {
        include!(concat!(env!("OUT_DIR"), "/lake.v1.rs"));
    }
    pub mod worker {
        pub mod v1 {
            include!(concat!(env!("OUT_DIR"), "/lake.worker.v1.rs"));
        }
    }
}
pub mod envelope {
    pub mod v1 {
        include!(concat!(env!("OUT_DIR"), "/envelope.v1.rs"));
    }
}
