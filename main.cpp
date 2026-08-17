#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdio>
#include <exception>
#include <functional>
#include <mutex>
#include <pthread.h>
#include <queue>
#include <thread>
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
//
// Note that the Refreshing flag alone is not what keeps resetAllContexts()
// from pulling contexts out from under a running decrypt -- see the barrier
// and the slot bookkeeping below. Sample decryption deliberately runs without
// kd_context_mutex held (that mutex only bounds context construction), so a
// bare flag check by the worker is a check-then-act with a blocking socket
// read in the middle.
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

// How long the barrier waits for in-flight decrypts to drain. A slot brackets
// a single decrypt call with no socket I/O inside it, so the drain normally
// takes microseconds; this only fires if a library call is wedged, and in
// that case deferring the refresh beats freeing contexts it is still using.
static const int kQuiesceTimeoutSecs = 5;

// Floor on how often a refresh may run, independent of the failure backoff.
// A success clears consec_fails, so without this a source that keeps firing
// right after each successful refresh would spin at full speed.
static const int kMinRefreshGapSecs = 1;

static std::mutex g_recovery_mtx;
static std::condition_variable g_recovery_cv;
static std::queue<int> g_recovery_q;

// Guards the in-flight decrypt count and the transition into Refreshing.
// Those two have to move under one lock: the point of the barrier is that
// once Refreshing is visible no further slot can be taken, so testing the
// state and taking a slot cannot be two separate steps.
static std::mutex g_ctx_mtx;
static std::condition_variable g_ctx_cv;
static int g_active_decrypts = 0;

// Bumped by refresh_decrypt_ctx_locked() under kd_context_mutex right after
// resetAllContexts(). A decrypt worker captures the generation together with
// its context (also under kd_context_mutex, so the pair is consistent) and
// re-checks it before every sample: a worker that acquired its context before
// a reset would otherwise resume afterwards and dereference freed memory.
static std::atomic<unsigned long long> g_ctx_generation{0};

extern "C" unsigned long long ctx_generation(void) {
    return g_ctx_generation.load();
}

extern "C" void bump_ctx_generation(void) { g_ctx_generation.fetch_add(1); }

namespace {

// Holds one in-flight decrypt slot for as long as it is in scope.
//
// The destructor is what makes the release unconditional. The decrypt call it
// wraps can throw, and main.c is compiled as C without landing pads, so a
// decrement written on the C side would be skipped on the throw path and the
// recovery worker would then wait out its whole timeout on a slot that is
// never coming back. Same reasoning as run_with_mutex above.
class DecryptSlot {
public:
    DecryptSlot() : held_(false) {}

    ~DecryptSlot() {
        if (!held_)
            return;
        {
            std::lock_guard<std::mutex> lk(g_ctx_mtx);
            --g_active_decrypts;
        }
        g_ctx_cv.notify_all();
    }

    // Refuses if a reset is in flight, or if gen is stale -- i.e. the context
    // the caller is holding was already freed by a reset that has finished.
    bool acquire(unsigned long long gen) {
        std::lock_guard<std::mutex> lk(g_ctx_mtx);
        if (g_recovery_state.load() == RecoveryState::Refreshing)
            return false;
        if (gen != g_ctx_generation.load())
            return false;
        ++g_active_decrypts;
        held_ = true;
        return true;
    }

    DecryptSlot(const DecryptSlot &) = delete;
    DecryptSlot &operator=(const DecryptSlot &) = delete;

private:
    bool held_;
};

} // namespace

// Runs fn(arg) -- one sample decryption -- holding a slot, or returns 0
// without calling it if the context is being or has been reset. Callers must
// treat 0 as "this connection is done": the context they hold is gone and the
// client has to re-request the key.
extern "C" int run_decrypt_guarded(unsigned long long gen, void (*fn)(void *),
                                   void *arg) {
    DecryptSlot slot;
    if (!slot.acquire(gen))
        return 0;
    fn(arg);
    return 1;
}

// Takes the FairPlay contexts offline: refuses new sample decrypts, then waits
// for the ones already inside the decrypt call to finish. resetAllContexts()
// frees every context, and decrypt workers run unlocked by design, so without
// this drain the reset races a decrypt on a pointer it is about to free.
//
// Returns false if the drain timed out, in which case the caller must not
// reset. The state is left as the caller's failure path sets it.
static bool begin_refresh_barrier() {
    std::unique_lock<std::mutex> lk(g_ctx_mtx);
    g_recovery_state.store(RecoveryState::Refreshing);
    if (g_ctx_cv.wait_for(lk, std::chrono::seconds(kQuiesceTimeoutSecs),
                          [] { return g_active_decrypts == 0; }))
        return true;
    fprintf(stderr,
            "[recovery] %d decrypt(s) still in flight after %ds, deferring "
            "reset\n",
            g_active_decrypts, kQuiesceTimeoutSecs);
    return false;
}

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
    bool have_refreshed = false;
    std::chrono::steady_clock::time_point last_refresh;

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
            std::this_thread::sleep_for(std::chrono::seconds(delay));
        }

        if (have_refreshed) {
            const auto min_gap = std::chrono::seconds(kMinRefreshGapSecs);
            const auto since = std::chrono::steady_clock::now() - last_refresh;
            if (since < min_gap)
                std::this_thread::sleep_for(min_gap - since);
        }

        fprintf(stderr, "[recovery] refreshing decrypt context\n");

        int ready = 0;
        if (begin_refresh_barrier()) {
            try {
                ready = refresh_decrypt_ctx();
            } catch (const std::exception &e) {
                fprintf(stderr, "[recovery] refresh threw: %s\n", e.what());
            } catch (...) {
                fprintf(stderr, "[recovery] refresh threw unknown exception\n");
            }
        }

        have_refreshed = true;
        last_refresh = std::chrono::steady_clock::now();

        if (ready) {
            consec_fails = 0;
            {
                std::lock_guard<std::mutex> lk(g_recovery_mtx);
                // Everything still queued fired before this reset finished, so
                // it is already answered: requestLease, resetAllContexts and a
                // fresh preshare context all happened after it. Re-running for
                // those would tear down the context just rebuilt, and since
                // success clears consec_fails the backoff above would not slow
                // the repeat down either. Events about the new lease arrive
                // from here on and get their own cycle.
                const size_t stale = g_recovery_q.size();
                if (stale > 0) {
                    fprintf(stderr,
                            "[recovery] dropping %zu event(s) superseded by "
                            "this refresh\n",
                            stale);
                    std::queue<int> drained;
                    g_recovery_q.swap(drained);
                }
            }
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
