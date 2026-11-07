//! One-way process boundary installed after trusted runtime initialization and
//! before reading any request. Failure is fatal to startup; never continue with
//! a partially installed limit or retry an operation outside the filter.

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SandboxError {
    #[cfg(not(all(target_os = "linux", target_arch = "x86_64")))]
    Unsupported,
    #[cfg(all(target_os = "linux", target_arch = "x86_64"))]
    Limit,
    #[cfg(all(target_os = "linux", target_arch = "x86_64"))]
    NoNewPrivileges,
    #[cfg(all(target_os = "linux", target_arch = "x86_64"))]
    Filter,
}

#[cfg(not(all(target_os = "linux", target_arch = "x86_64")))]
pub fn install() -> Result<(), SandboxError> {
    Err(SandboxError::Unsupported)
}

#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
pub fn install() -> Result<(), SandboxError> {
    linux::install()
}

#[cfg(all(target_os = "linux", target_arch = "x86_64"))]
mod linux {
    use super::SandboxError;
    use libc::sock_filter;

    // seccomp_data offsets for Linux x86_64 (u32 nr, u32 arch, u64 IP,
    // six u64 arguments). Check architecture before interpreting any argument.
    const NR: u32 = 0;
    const ARCH: u32 = 4;
    const ARG: u32 = 16;
    const AUDIT_X86_64: u32 = 0xc000_003e;
    const X32: u32 = 0x4000_0000;
    const ALLOW: u32 = 0x7fff_0000;
    const KILL_PROCESS: u32 = 0x8000_0000;
    const DENY: u32 = 0x0005_0000 | libc::EPERM as u32;
    const LD: u16 = 0x20; // BPF_LD | BPF_W | BPF_ABS
    const JEQ: u16 = 0x15;
    const JGT: u16 = 0x25;
    const JSET: u16 = 0x45;
    const RET: u16 = 0x06;

    fn instruction(code: u16, jt: u8, jf: u8, k: u32) -> sock_filter {
        sock_filter { code, jt, jf, k }
    }
    fn load(offset: u32) -> sock_filter {
        instruction(LD, 0, 0, offset)
    }
    fn ret(value: u32) -> sock_filter {
        instruction(RET, 0, 0, value)
    }
    fn equal(out: &mut Vec<sock_filter>, value: u32) {
        out.push(instruction(JEQ, 1, 0, value));
        out.push(ret(DENY));
    }
    fn maximum(out: &mut Vec<sock_filter>, value: u32) {
        out.push(instruction(JGT, 0, 1, value));
        out.push(ret(DENY));
    }
    fn unsigned(out: &mut Vec<sock_filter>, index: u32) {
        out.push(load(ARG + index * 8 + 4));
        equal(out, 0);
        out.push(load(ARG + index * 8));
    }
    fn choices(out: &mut Vec<sock_filter>, values: &[u32]) {
        for value in values {
            out.push(instruction(JEQ, 0, 1, *value));
            out.push(ret(ALLOW));
        }
        out.push(ret(DENY));
    }
    fn rule(out: &mut Vec<sock_filter>, syscall: libc::c_long, mut body: Vec<sock_filter>) {
        // Each branch ends in RET, so the next rule always reloads the syscall.
        assert!(body.len() <= u8::MAX as usize);
        out.push(load(NR));
        out.push(instruction(JEQ, 0, body.len() as u8, syscall as u32));
        out.append(&mut body);
    }
    fn protection(out: &mut Vec<sock_filter>, index: u32) {
        unsigned(out, index);
        // Only NONE/READ/WRITE combinations; executable mappings are forbidden.
        maximum(out, (libc::PROT_READ | libc::PROT_WRITE) as u32);
    }

    fn filter() -> Vec<sock_filter> {
        let mut out = vec![
            load(ARCH),
            instruction(JEQ, 1, 0, AUDIT_X86_64),
            ret(KILL_PROCESS),
            load(NR),
            instruction(JSET, 0, 1, X32),
            ret(KILL_PROCESS),
        ];
        for (syscall, descriptors) in [
            (libc::SYS_read, &[0][..]),
            (libc::SYS_write, &[1, 2][..]),
            (libc::SYS_fstat, &[0, 1, 2][..]),
        ] {
            let mut body = Vec::new();
            unsigned(&mut body, 0);
            choices(&mut body, descriptors);
            rule(&mut out, syscall, body);
        }
        let mut mmap = Vec::new();
        protection(&mut mmap, 2);
        unsigned(&mut mmap, 3);
        equal(&mut mmap, (libc::MAP_PRIVATE | libc::MAP_ANONYMOUS) as u32);
        // fd is a signed 32-bit int. Accept its zero- or sign-extended ABI
        // representation only; both name -1, never an existing descriptor.
        mmap.push(load(ARG + 4 * 8));
        equal(&mut mmap, u32::MAX);
        mmap.push(load(ARG + 4 * 8 + 4));
        mmap.push(instruction(JEQ, 2, 0, 0));
        equal(&mut mmap, u32::MAX);
        unsigned(&mut mmap, 5);
        equal(&mut mmap, 0);
        mmap.push(ret(ALLOW));
        rule(&mut out, libc::SYS_mmap, mmap);
        let mut protect = Vec::new();
        protection(&mut protect, 2);
        protect.push(ret(ALLOW));
        rule(&mut out, libc::SYS_mprotect, protect);
        let mut remap = Vec::new();
        unsigned(&mut remap, 3);
        choices(&mut remap, &[0, libc::MREMAP_MAYMOVE as u32]);
        rule(&mut out, libc::SYS_mremap, remap);
        let mut futex = Vec::new();
        unsigned(&mut futex, 1);
        choices(
            &mut futex,
            &[
                (libc::FUTEX_WAIT | libc::FUTEX_PRIVATE_FLAG) as u32,
                (libc::FUTEX_WAKE | libc::FUTEX_PRIVATE_FLAG) as u32,
                (libc::FUTEX_WAIT_BITSET | libc::FUTEX_PRIVATE_FLAG) as u32,
                (libc::FUTEX_WAKE_BITSET | libc::FUTEX_PRIVATE_FLAG) as u32,
            ],
        );
        rule(&mut out, libc::SYS_futex, futex);
        let mut clock = Vec::new();
        unsigned(&mut clock, 0);
        choices(
            &mut clock,
            &[libc::CLOCK_REALTIME as u32, libc::CLOCK_MONOTONIC as u32],
        );
        rule(&mut out, libc::SYS_clock_gettime, clock);
        let mut random = Vec::new();
        unsigned(&mut random, 1);
        maximum(&mut random, 256);
        unsigned(&mut random, 2);
        choices(&mut random, &[0, libc::GRND_NONBLOCK]);
        rule(&mut out, libc::SYS_getrandom, random);
        for syscall in [
            libc::SYS_brk,
            libc::SYS_munmap,
            libc::SYS_sched_yield,
            libc::SYS_rt_sigreturn,
            libc::SYS_exit,
            libc::SYS_exit_group,
        ] {
            rule(&mut out, syscall, vec![ret(ALLOW)]);
        }
        out.push(ret(DENY));
        out
    }

    pub(super) fn install() -> Result<(), SandboxError> {
        // Preconstruct the filter while trusted initialization may still allocate.
        let mut instructions = filter();
        let program = libc::sock_fprog {
            len: u16::try_from(instructions.len()).map_err(|_| SandboxError::Filter)?,
            filter: instructions.as_mut_ptr(),
        };
        for (resource, value) in [
            (libc::RLIMIT_AS, 512 * 1024 * 1024),
            (libc::RLIMIT_CPU, 20),
            (libc::RLIMIT_CORE, 0),
            (libc::RLIMIT_NOFILE, 16),
        ] {
            let limit = libc::rlimit {
                rlim_cur: value,
                rlim_max: value,
            };
            // SAFETY: valid live rlimit; no pointer escapes the syscall.
            if unsafe { libc::setrlimit(resource, &limit) } != 0 {
                return Err(SandboxError::Limit);
            }
        }
        // SAFETY: fixed scalar prctl arguments, unused arguments explicitly zero.
        if unsafe { libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) } != 0 {
            return Err(SandboxError::NoNewPrivileges);
        }
        // SAFETY: program and backing instructions remain live through the call.
        // TSYNC applies to all existing threads; positive offending-TID returns
        // are failures too. No thread/process creation is allowed afterwards.
        let result = unsafe {
            libc::syscall(
                libc::SYS_seccomp,
                libc::SECCOMP_SET_MODE_FILTER,
                libc::SECCOMP_FILTER_FLAG_TSYNC,
                &program,
            )
        };
        if result != 0 {
            return Err(SandboxError::Filter);
        }
        Ok(())
    }

    #[cfg(test)]
    mod tests {
        use super::*;

        // Exercise the emitted classic-BPF branches without installing a
        // process-wide filter in the test runner. Actual kernel enforcement is
        // a separate subprocess qualification, including successful worker IO.
        fn verdict(arch: u32, nr: i64, args: [u64; 6]) -> u32 {
            let code = filter();
            let mut data = [0_u32; 16];
            data[0] = nr as u32;
            data[1] = arch;
            for (i, value) in args.into_iter().enumerate() {
                data[4 + i * 2] = value as u32;
                data[5 + i * 2] = (value >> 32) as u32;
            }
            let (mut pc, mut a) = (0, 0);
            for _ in 0..code.len() {
                let instruction = &code[pc];
                match instruction.code {
                    LD => a = data[instruction.k as usize / 4],
                    RET => return instruction.k,
                    JEQ | JGT | JSET => {
                        let yes = match instruction.code {
                            JEQ => a == instruction.k,
                            JGT => a > instruction.k,
                            _ => a & instruction.k != 0,
                        };
                        pc += usize::from(if yes { instruction.jt } else { instruction.jf });
                    }
                    _ => panic!("unexpected filter instruction"),
                }
                pc += 1;
            }
            panic!("filter did not return")
        }

        #[test]
        fn denies_authority_and_abi_bypasses() {
            for syscall in [
                libc::SYS_open,
                libc::SYS_openat,
                libc::SYS_openat2,
                libc::SYS_socket,
                libc::SYS_connect,
                libc::SYS_execve,
                libc::SYS_execveat,
                libc::SYS_clone,
                libc::SYS_clone3,
                libc::SYS_fork,
                libc::SYS_vfork,
                libc::SYS_ptrace,
                libc::SYS_process_vm_readv,
                libc::SYS_io_uring_setup,
            ] {
                assert_eq!(verdict(AUDIT_X86_64, syscall, [0; 6]), DENY);
            }
            assert_eq!(verdict(0x4000_0003, libc::SYS_read, [0; 6]), KILL_PROCESS);
            assert_eq!(verdict(AUDIT_X86_64, i64::from(X32), [0; 6]), KILL_PROCESS);
            assert_eq!(verdict(AUDIT_X86_64, libc::SYS_read, [0; 6]), ALLOW);
            assert_eq!(
                verdict(AUDIT_X86_64, libc::SYS_read, [1, 0, 0, 0, 0, 0]),
                DENY
            );
            assert_eq!(
                verdict(AUDIT_X86_64, libc::SYS_write, [2, 0, 0, 0, 0, 0]),
                ALLOW
            );
            assert_eq!(
                verdict(AUDIT_X86_64, libc::SYS_write, [3, 0, 0, 0, 0, 0]),
                DENY
            );
            assert_eq!(
                verdict(
                    AUDIT_X86_64,
                    libc::SYS_write,
                    [(1 << 32) | 1, 0, 0, 0, 0, 0]
                ),
                DENY
            );
        }

        #[test]
        fn bounds_memory_mapping_and_entropy_arguments() {
            let mapping = [
                0,
                4096,
                3,
                (libc::MAP_PRIVATE | libc::MAP_ANONYMOUS) as u64,
                u64::MAX,
                0,
            ];
            assert_eq!(verdict(AUDIT_X86_64, libc::SYS_mmap, mapping), ALLOW);
            let mut zero_extended = mapping;
            zero_extended[4] = u64::from(u32::MAX);
            assert_eq!(verdict(AUDIT_X86_64, libc::SYS_mmap, zero_extended), ALLOW);
            for (index, value) in [
                (2, 7),
                (3, libc::MAP_PRIVATE as u64),
                (3, (libc::MAP_SHARED | libc::MAP_ANONYMOUS) as u64),
                (4, 0),
                (4, (1_u64 << 32) | u64::from(u32::MAX)),
                (5, 4096),
            ] {
                let mut bad = mapping;
                bad[index] = value;
                assert_eq!(verdict(AUDIT_X86_64, libc::SYS_mmap, bad), DENY);
            }
            for flags in [0, u64::from(libc::GRND_NONBLOCK)] {
                assert_eq!(
                    verdict(AUDIT_X86_64, libc::SYS_getrandom, [0, 256, flags, 0, 0, 0]),
                    ALLOW
                );
            }
            for (length, flags) in [(257, 0), ((1 << 32) | 1, 0), (1, 2)] {
                assert_eq!(
                    verdict(
                        AUDIT_X86_64,
                        libc::SYS_getrandom,
                        [0, length, flags, 0, 0, 0]
                    ),
                    DENY
                );
            }
            assert_eq!(
                verdict(AUDIT_X86_64, libc::SYS_mprotect, [0, 4096, 4, 0, 0, 0]),
                DENY
            );
        }
    }
}
