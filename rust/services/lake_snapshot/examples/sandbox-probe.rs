//! Closed, synthetic kernel qualification process. No worker production flags.
#[path = "../src/sandbox.rs"]
mod sandbox;

#[cfg(not(all(target_os = "linux", target_arch = "x86_64")))]
fn main() {
    assert_eq!(sandbox::install(), Err(sandbox::SandboxError::Unsupported));
    std::process::exit(2);
}

#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
fn main() {
    linux::main();
}

#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
mod linux {
    use std::io::Write;

    fn stamp() -> &'static str {
        option_env!("KANZ_BUILD_VERSION").unwrap_or("unversioned")
    }
    fn profile() -> &'static str {
        if cfg!(debug_assertions) {
            "debug"
        } else {
            "release"
        }
    }
    fn denied(result: libc::c_long) -> bool {
        result == -1 && std::io::Error::last_os_error().raw_os_error() == Some(libc::EPERM)
    }
    fn check(ok: bool) {
        if !ok {
            std::process::exit(3);
        }
    }
    pub(super) fn main() {
        let arguments: Vec<String> = std::env::args().collect();
        if arguments.len() != 2
            || !matches!(arguments[1].as_str(), "checks" | "cpu")
            || stamp().len() != 40
            || !stamp().bytes().all(|b| b.is_ascii_hexdigit())
        {
            std::process::exit(2);
        }
        // Match production initialization; no filesystem or network driver.
        let _runtime = tokio::runtime::Builder::new_current_thread()
            .build()
            .unwrap();
        check(super::sandbox::install().is_ok());
        if arguments[1] == "cpu" {
            println!("sandbox-cpu {} {}", stamp(), profile());
            std::io::stdout().flush().unwrap();
            loop {
                std::hint::black_box(1_u64.wrapping_mul(7));
            }
        }
        // All syscall probes occur after installation. Invalid resources make
        // EPERM distinguishable from ordinary EBADF/ENOENT/EINVAL failures.
        unsafe {
            check(denied(libc::syscall(
                libc::SYS_openat,
                libc::AT_FDCWD,
                c"/kanz-sandbox-does-not-exist".as_ptr(),
                libc::O_RDONLY,
                0,
            )));
            check(denied(libc::syscall(
                libc::SYS_socket,
                libc::AF_INET,
                libc::SOCK_STREAM,
                0,
            )));
            check(denied(libc::syscall(
                libc::SYS_connect,
                -1,
                std::ptr::null::<libc::sockaddr>(),
                0,
            )));
            check(denied(libc::syscall(
                libc::SYS_execve,
                c"/kanz-sandbox-does-not-exist".as_ptr(),
                std::ptr::null::<*const libc::c_char>(),
                std::ptr::null::<*const libc::c_char>(),
            )));
            check(denied(libc::syscall(
                libc::SYS_clone3,
                std::ptr::null::<libc::c_void>(),
                0,
            )));
            let fork = libc::syscall(libc::SYS_fork);
            if fork == 0 {
                libc::_exit(90);
            }
            check(denied(fork));
            check(denied(libc::syscall(
                libc::SYS_io_uring_setup,
                1,
                std::ptr::null::<libc::c_void>(),
            )));
            let mut bytes = [0_u8; 257];
            check(denied(libc::syscall(
                libc::SYS_read,
                1_u64 << 32,
                bytes.as_mut_ptr(),
                0,
            )));
            check(denied(libc::syscall(libc::SYS_write, 3, bytes.as_ptr(), 0)));
            check(libc::read(0, bytes.as_mut_ptr().cast(), 0) == 0);
            check(libc::write(1, bytes.as_ptr().cast(), 0) == 0);
            check(libc::write(2, bytes.as_ptr().cast(), 0) == 0);
            let flags = libc::MAP_PRIVATE | libc::MAP_ANONYMOUS;
            let memory = libc::mmap(
                std::ptr::null_mut(),
                4096,
                libc::PROT_READ | libc::PROT_WRITE,
                flags,
                -1,
                0,
            );
            check(memory != libc::MAP_FAILED);
            memory.cast::<u8>().write_volatile(42);
            check(memory.cast::<u8>().read_volatile() == 42);
            check(denied(
                libc::mprotect(memory, 4096, libc::PROT_READ | libc::PROT_EXEC) as libc::c_long,
            ));
            check(libc::munmap(memory, 4096) == 0);
            check(denied(libc::syscall(
                libc::SYS_mmap,
                0,
                4096,
                libc::PROT_EXEC,
                flags,
                -1_i64,
                0,
            )));
            check(denied(libc::syscall(
                libc::SYS_mmap,
                0,
                4096,
                libc::PROT_READ,
                flags,
                0,
                0,
            )));
            check(libc::syscall(libc::SYS_getrandom, bytes.as_mut_ptr(), 256_usize, 0_u32) == 256);
            check(denied(libc::syscall(
                libc::SYS_getrandom,
                bytes.as_mut_ptr(),
                257_usize,
                0_u32,
            )));
            let over = libc::mmap(
                std::ptr::null_mut(),
                513 * 1024 * 1024,
                libc::PROT_READ | libc::PROT_WRITE,
                flags,
                -1,
                0,
            );
            check(
                over == libc::MAP_FAILED
                    && std::io::Error::last_os_error().raw_os_error() == Some(libc::ENOMEM),
            );
        }
        println!("sandbox-checks {} {} 21", stamp(), profile());
        std::process::exit(0);
    }
}
