#include <atomic>
#include <condition_variable>
#include <cstdio>
#include <exception>
#include <functional>
#include <mutex>
#include <pthread.h>
#include <queue>
#include <thread>
#include <unistd.h>
#include <vector>

extern "C" int refresh_decrypt_ctx(void);
extern "C" void handle(int fd);

namespace {

// RAII guard over a plain pthread_mutex_t. The mutex itself has to stay a
// pthread_mutex_t (not a std::mutex) because it is declared and used from
// main.c, which is compiled as C.
class PthreadMutexGuard {
public:
    explicit PthreadMutexGuard(pthread_mutex_t *m) : m_(m) {
        pthread_mutex_lock(m_);
    }
    ~PthreadMutexGuard() { pthread_mutex_unlock(m_); }

    PthreadMutexGuard(const PthreadMutexGuard &) = delete;
    PthreadMutexGuard &operator=(const PthreadMutexGuard &) = delete;

private:
    pthread_mutex_t *m_;
};

} // namespace

// Runs fn(arg) with *m held and releases it on every exit path, including a
// C++ exception thrown out of fn.
//
// This has to live in the C++ translation unit. main.c is compiled as C
// without -fexceptions, so it gets no landing pads: when a libandroidappmusic
// call throws, the unwinder walks straight through the C frames and every
// pthread_mutex_unlock written there is skipped, leaking the mutex for the
// life of the process. Owning the lock in this frame is what makes the
// release unconditional -- the guard's destructor runs during unwinding.
//
// The exception is deliberately not caught here, so it still reaches
// handle_cpp below and the connection still fails the way it always has.
// Only the mutex leak is fixed.
extern "C" void run_with_mutex(pthread_mutex_t *m, void (*fn)(void *),
                               void *arg) {
    PthreadMutexGuard guard(m);
    fn(arg);
}

// ---------------------------------------------------------------------------
// Lease recovery
//
// Callbacks only enqueue. A dedicated worker owns refresh_decrypt_ctx() so
// FairPlay/library calls never run on the lease-manager thread.
//
// Incoming decrypt/m3u8 work is refused only while Refreshing (the reset is
// actually in flight). Scheduled/Failed still accept requests.
// ---------------------------------------------------------------------------

enum class RecoveryState : int {
    Running = 0,
    Scheduled = 1,
    Refreshing = 2,
    Failed = 3,
};

static std::atomic<RecoveryState> g_recovery_state{RecoveryState::Running};

extern "C" int is_refreshing(void) {
    return g_recovery_state.load() == RecoveryState::Refreshing ? 1 : 0;
}

static const int kPlaybackErr = -1;
static const int kRetryInternal = -2;
static const int kDecryptException = -3;

static const int kBackoffSecs[] = {1, 2, 5, 10, 30};
static const int kBackoffMaxIdx = 4;

static std::mutex g_recovery_mtx;
static std::condition_variable g_recovery_cv;
static std::queue<int> g_recovery_q;

static void schedule_recovery(int code) {
    {
        std::lock_guard<std::mutex> lk(g_recovery_mtx);
        g_recovery_q.push(code);
    }
    g_recovery_cv.notify_one();
}

extern "C" void request_lease_recovery(void) {
    if (g_recovery_state.load() != RecoveryState::Running)
        return;
    fprintf(stderr, "[recovery] decrypt exception - scheduling recovery\n");
    schedule_recovery(kDecryptException);
}

static void log_recovery_codes(const std::vector<int> &burst) {
    fprintf(stderr, "[recovery] %zu event(s):", burst.size());
    for (size_t i = 0; i < burst.size(); ++i) {
        const int c = burst[i];
        if (c == kPlaybackErr)
            fprintf(stderr, " PLAYBACK_ERROR");
        else if (c == kRetryInternal)
            fprintf(stderr, " RETRY");
        else if (c == kDecryptException)
            fprintf(stderr, " DECRYPT_EXCEPTION");
        else
            fprintf(stderr, " LEASE_END=%d", c);
    }
    fprintf(stderr, "\n");
}

static void recovery_worker() {
    int consec_fails = 0;

    while (true) {
        std::vector<int> burst;
        {
            std::unique_lock<std::mutex> lk(g_recovery_mtx);
            g_recovery_cv.wait(lk, [] { return !g_recovery_q.empty(); });
            while (!g_recovery_q.empty()) {
                burst.push_back(g_recovery_q.front());
                g_recovery_q.pop();
            }
        }

        g_recovery_state.store(RecoveryState::Scheduled);
        log_recovery_codes(burst);

        if (consec_fails > 0) {
            int idx = consec_fails - 1;
            if (idx > kBackoffMaxIdx)
                idx = kBackoffMaxIdx;
            const int delay = kBackoffSecs[idx];
            fprintf(stderr, "[recovery] backoff %ds (fail=%d)\n", delay,
                    consec_fails);
            sleep(static_cast<unsigned int>(delay));
        }

        g_recovery_state.store(RecoveryState::Refreshing);
        fprintf(stderr, "[recovery] refreshing decrypt context\n");

        int ready = 0;
        try {
            ready = refresh_decrypt_ctx();
        } catch (const std::exception &e) {
            fprintf(stderr, "[recovery] refresh threw: %s\n", e.what());
        } catch (...) {
            fprintf(stderr, "[recovery] refresh threw unknown exception\n");
        }

        if (ready) {
            consec_fails = 0;
            g_recovery_state.store(RecoveryState::Running);
            fprintf(stderr, "[recovery] refresh ok, resuming\n");
        } else {
            consec_fails++;
            g_recovery_state.store(RecoveryState::Failed);
            fprintf(stderr, "[recovery] refresh failed (consecutive=%d)\n",
                    consec_fails);
            schedule_recovery(kRetryInternal);
        }
    }
}

extern "C" void start_recovery_thread(void) {
    std::thread(recovery_worker).detach();
    fprintf(stderr, "[+] recovery thread started\n");
}

extern "C" uint8_t handle_cpp(int fd) {
    try {
        handle(fd);
        return 1;
    } catch (const std::exception &e) {
        fprintf(stderr, "[!] catched an exception: %s\n", e.what());
        return 0;
    }
}

static void endLeaseCb(int const &c) {
    fprintf(stderr, "[.] end lease code %d - scheduling recovery\n", c);
    schedule_recovery(c);
}

static void pbErrCb(void *) {
    fprintf(stderr, "[.] playback error - scheduling recovery\n");
    schedule_recovery(kPlaybackErr);
}

extern "C" std::function<void(int const &)> endLeaseCallback(endLeaseCb);
extern "C" std::function<void(void *)> pbErrCallback(pbErrCb);
