//! Build the example first with the same KANZ_BUILD_VERSION/profile as this test.
//! Every kernel probe runs in a separate process; never sandbox the test runner.
#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
mod linux {
    use std::io::Read;
    use std::os::unix::process::ExitStatusExt;
    use std::path::PathBuf;
    use std::process::{Command, Stdio};
    use std::time::{Duration, Instant};

    struct OwnedChild(std::process::Child);
    impl std::ops::Deref for OwnedChild {
        type Target = std::process::Child;
        fn deref(&self) -> &Self::Target {
            &self.0
        }
    }
    impl std::ops::DerefMut for OwnedChild {
        fn deref_mut(&mut self) -> &mut Self::Target {
            &mut self.0
        }
    }
    impl Drop for OwnedChild {
        fn drop(&mut self) {
            if !matches!(self.0.try_wait(), Ok(Some(_))) {
                let _ = self.0.kill();
                let _ = self.0.wait();
            }
        }
    }

    fn source() -> &'static str {
        let value = option_env!("KANZ_BUILD_VERSION").unwrap_or("unversioned");
        assert!(
            value.len() == 40 && value.bytes().all(|b| b.is_ascii_hexdigit()),
            "exact source stamp required"
        );
        value
    }
    fn profile() -> &'static str {
        if cfg!(debug_assertions) {
            "debug"
        } else {
            "release"
        }
    }
    fn executable() -> PathBuf {
        let current = std::env::current_exe().expect("test executable");
        let deps = current.parent().expect("deps directory");
        assert_eq!(deps.file_name().unwrap(), "deps");
        let target = deps.parent().expect("profile directory");
        assert_eq!(target.file_name().unwrap(), profile());
        let path = target.join("examples/sandbox-probe");
        let metadata = std::fs::symlink_metadata(&path)
            .expect("build --example sandbox-probe before running integration tests");
        assert!(metadata.is_file() && !metadata.file_type().is_symlink());
        path
    }
    fn capture(stream: impl Read + Send + 'static) -> std::thread::JoinHandle<Vec<u8>> {
        std::thread::spawn(move || {
            let mut bytes = Vec::new();
            stream
                .take(1025)
                .read_to_end(&mut bytes)
                .expect("bounded child output");
            bytes
        })
    }
    fn run(mode: &str) -> (std::process::ExitStatus, String, bool) {
        assert!(matches!(mode, "checks" | "cpu"));
        let mut child = OwnedChild(
            Command::new(executable())
                .arg(mode)
                .env_clear()
                .stdin(Stdio::null())
                .stdout(Stdio::piped())
                .stderr(Stdio::piped())
                .spawn()
                .expect("owned probe process"),
        );
        let stdout = capture(child.stdout.take().unwrap());
        let stderr = capture(child.stderr.take().unwrap());
        let deadline = Instant::now() + Duration::from_secs(25);
        let mut limits_seen = false;
        let mut parent_killed = false;
        let status = loop {
            // Read only this owned child's public resource limits; no environ,
            // command line, maps, file descriptors or configuration are read.
            if let Ok(limits) = std::fs::read_to_string(format!("/proc/{}/limits", child.id())) {
                let expected = [
                    ("Max cpu time", "20", "20"),
                    ("Max core file size", "0", "0"),
                    ("Max open files", "16", "16"),
                    ("Max address space", "536870912", "536870912"),
                ];
                limits_seen |= expected.iter().all(|(name, soft, hard)| {
                    limits.lines().any(|line| {
                        line.strip_prefix(name).is_some_and(|tail| {
                            let mut parts = tail.split_whitespace();
                            parts.next() == Some(*soft) && parts.next() == Some(*hard)
                        })
                    })
                });
            }
            if let Some(status) = child.try_wait().expect("observe owned child") {
                break status;
            }
            if Instant::now() >= deadline {
                parent_killed = true;
                child.kill().expect("kill owned timed-out child");
                break child.wait().expect("join owned timed-out child");
            }
            std::thread::sleep(Duration::from_millis(10));
        };
        let output = stdout.join().expect("join output reader");
        let error = stderr.join().expect("join error reader");
        assert!(
            !parent_killed,
            "probe exceeded parent deadline; not a kernel-limit pass"
        );
        assert!(
            output.len() <= 1024 && error.is_empty(),
            "unexpected bounded probe output"
        );
        (
            status,
            String::from_utf8(output).expect("fixed ASCII output"),
            limits_seen,
        )
    }
    #[test]
    fn actual_kernel_syscall_boundaries() {
        let expected = format!("sandbox-checks {} {} 21\n", source(), profile());
        let (status, output, _) = run("checks");
        assert!(
            status.success(),
            "kernel syscall controls refused or bypassed"
        );
        assert_eq!(output, expected);
    }
    #[test]
    fn actual_kernel_cpu_limit_and_resource_readback() {
        let expected = format!("sandbox-cpu {} {}\n", source(), profile());
        let (status, output, limits_seen) = run("cpu");
        assert_eq!(output, expected);
        assert!(
            limits_seen,
            "all four hard/soft limits must be observed on the owned child"
        );
        assert_eq!(
            status.signal(),
            Some(libc::SIGKILL),
            "CPU hard-limit process termination required"
        );
    }
}
