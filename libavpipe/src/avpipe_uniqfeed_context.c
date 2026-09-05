#include "avpipe_xc.h"

static __thread xctx_t *avpipe_uniqfeed_current_xctx;

void
avpipe_uniqfeed_provider_set_current_xctx(
    xctx_t *xctx)
{
    avpipe_uniqfeed_current_xctx = xctx;
}

void
avpipe_uniqfeed_provider_clear_current_xctx(void)
{
    avpipe_uniqfeed_current_xctx = NULL;
}

xctx_t *
avpipe_uniqfeed_provider_current_xctx(void)
{
    return avpipe_uniqfeed_current_xctx;
}